package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	openai "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/deployment"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/gorilla/websocket"
)

// nolint:gocyclo
func (h *WebSocketHandler) Handle(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetUserClaims(r.Context())
	connectionLimiter := h.connections
	if connectionLimiter == nil {
		connectionLimiter = getSharedWebSocketConnectionLimiter()
	}
	releaseConnection, acquired := acquireWebSocketConnection(
		w,
		r,
		claims,
		connectionLimiter,
	)
	if !acquired {
		return
	}
	defer releaseConnection()

	state, allowed := h.prepareTranslationState(w, r, claims)
	if !allowed {
		return
	}

	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}
	safeConn := newSafeWebSocketConn(conn)
	defer func() { _ = safeConn.Close() }()
	conn.SetReadLimit(translationMaxMessageSize)
	if err := configureTranslationReadLiveness(conn, translationPongWait); err != nil {
		log.Printf("Failed to configure WebSocket liveness: %v", err)
		return
	}

	log.Printf("WebSocket connection established from %s", strconv.Quote(r.RemoteAddr))

	// Get user ID from JWT if available (for billing)
	var userID, tenantID string
	if claims != nil {
		userID = claims.UserID
		tenantID = claims.TenantID
	}
	meteredProviderFlow := h.billing != nil && userID != "" && tenantID != ""
	replayBilling, _ := h.billing.(translationReplayBillingService)

	state.meteredRAGIngest = meteredProviderFlow
	newRAGService := h.newRAGService
	if newRAGService == nil {
		newRAGService = rag.NewServiceFromEnv
	}
	ragSvc, err := newRAGService()
	if err != nil {
		log.Printf("RAG init error: %v", err)
	} else {
		state.ragSvc = ragSvc
		if state.meteredRAGIngest {
			// Internal paragraph summarization does not return usage, so it
			// cannot be reconciled safely. Incremental summaries remain
			// available through the separately reserved/billed path.
			ragSvc.SetIngestSummarizeEnabled(false)
		}
		defer func() { _ = ragSvc.Close() }()
	}
	// Keep already accepted, billed work alive across a transient socket
	// disconnect. The handler still applies strict provider and teardown
	// deadlines below, while context values (auth/tracing) remain available.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	stopHandoff := deployment.Default.NotifyHandoff(safeConn.WriteJSON)
	defer stopHandoff()
	deliveryWaitCtx, stopDeliveryWait := context.WithCancel(ctx)
	defer stopDeliveryWait()
	paidCtx, stopPaidFlow := context.WithCancel(ctx)
	defer stopPaidFlow()
	var paidFlow providerFlowGate
	var paidFlowCloseOnce sync.Once
	closePaidFlow := func(failure websocketAccountingFailure) {
		paidFlowCloseOnce.Do(func() {
			_ = safeConn.WriteJSON(failure.response())
			_ = safeConn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(
					websocket.CloseTryAgainLater,
					failure.ErrorType,
				),
			)
			_ = safeConn.Close()
		})
	}
	tripProviderFlow := func(
		failure websocketAccountingFailure,
	) (websocketAccountingFailure, bool) {
		if !meteredProviderFlow {
			return websocketAccountingFailure{}, false
		}
		failure, first := paidFlow.FailClosed(failure)
		if !first {
			return failure, false
		}
		log.Printf(
			"tripped metered WebSocket after uncertain provider accounting: type=%s cause=%v",
			failure.ErrorType,
			failure.Cause,
		)
		return failure, true
	}
	tripPaidFlow := func(cause error) (websocketAccountingFailure, bool) {
		return tripProviderFlow(accountingUncertainFailure(cause))
	}
	failClosePaidFlow := func(cause error) {
		if failure, first := tripPaidFlow(cause); first {
			closePaidFlow(failure)
		}
	}
	stopHeartbeat := startTranslationHeartbeat(ctx, safeConn)

	// Translation concurrency: queue + workers + in-order delivery.
	jobs := make(chan translateJob, 128)
	results := make(chan translateResult, 128)
	var nextSeq atomic.Int64
	var workerWG sync.WaitGroup
	var deliveryWG sync.WaitGroup
	var replayWG sync.WaitGroup
	var barrierWG sync.WaitGroup
	var auxiliaryWG sync.WaitGroup
	var workersOnce sync.Once
	var activeWorkers int
	var auxiliaryNext atomic.Int64
	deliveryProgress := newSequenceProgress()
	auxiliaryProgress := newSequenceProgress()
	auxiliaryTasks := make(chan auxiliaryTask, 64)
	flushBarrierSlots := make(chan struct{}, 8)

	const auxiliaryWorkers = 4
	auxiliaryWG.Add(auxiliaryWorkers)
	for i := 0; i < auxiliaryWorkers; i++ {
		go func() {
			defer auxiliaryWG.Done()
			for task := range auxiliaryTasks {
				if ctx.Err() == nil && task.run != nil {
					task.run()
				}
				auxiliaryProgress.Mark(task.seq)
			}
		}()
	}

	enqueueAuxiliary := func(run func()) {
		sequence := auxiliaryNext.Add(1)
		task := auxiliaryTask{seq: sequence, run: run}
		timer := time.NewTimer(websocketQueueWait)
		defer timer.Stop()
		select {
		case auxiliaryTasks <- task:
			return
		case <-ctx.Done():
			// Preserve a contiguous barrier even when cancellation wins after
			// allocating the sequence.
			auxiliaryProgress.Mark(sequence)
			return
		case <-timer.C:
			auxiliaryProgress.Mark(sequence)
			log.Printf("closing WebSocket because the auxiliary work queue remained full")
			cancel()
			_ = safeConn.Close()
			return
		}
	}

	sendResult := func(result translateResult) bool {
		select {
		case results <- result:
			return true
		case <-ctx.Done():
			return false
		}
	}
	closePaidFlowAfterDelivery := func(
		failure websocketAccountingFailure,
		target int64,
	) {
		barrierWG.Add(1)
		go func() {
			defer barrierWG.Done()
			waitCtx, stopWait := context.WithTimeout(
				context.Background(),
				writeWait+time.Second,
			)
			delivered := deliveryProgress.Wait(waitCtx, target)
			stopWait()
			if !delivered {
				log.Printf(
					"closing metered WebSocket after delivery barrier %d timed out",
					target,
				)
			}
			closePaidFlow(failure)
		}()
	}

	startWorkers := func(count int) {
		workersOnce.Do(func() {
			activeWorkers = count
			for i := 0; i < count; i++ {
				workerWG.Add(1)
				go func() {
					defer workerWG.Done()
					for {
						select {
						case <-ctx.Done():
							return
						case job, ok := <-jobs:
							if !ok {
								return
							}
							sendJobResult := func(result translateResult) bool {
								if job.cancelOperation != nil {
									job.cancelOperation()
								}
								result.seq = job.seq
								result.requestID = job.requestID
								h.translationRequests.Complete(
									job.requestCacheKey,
									job.requestCacheItem,
									&result,
									time.Now(),
								)
								return sendResult(result)
							}

							if !job.submittedAt.IsZero() &&
								time.Since(job.submittedAt) >= translationEndToEndBudget {
								if !sendJobResult(translateResult{
									seq: job.seq, speaker: job.speaker, original: job.text,
									startTime: job.startTime, endTime: job.endTime,
									err:          fmt.Errorf("translation request exceeded its queue budget"),
									errorType:    "translation_processing",
									retryAfterMs: int(translationProcessingRetry / time.Millisecond),
									retryable:    true,
								}) {
									return
								}
								continue
							}

							sessionID := billingSessionReference(job.sessionID)
							var reservation *realtimeUsageReservation
							var durableClaim *billing.TranslationRequestClaim
							operationCtx := job.operationCtx
							if operationCtx == nil {
								operationCtx = ctx
							}
							if replayBilling != nil && job.requestKey != "" && userID != "" {
								claim, claimErr := replayBilling.ClaimTranslationRequest(
									operationCtx,
									job.requestKey,
									job.requestFingerprint,
									&billing.UsageRecord{
										UserID: userID, TenantID: tenantID, SessionID: sessionID,
										Action: "translation",
									},
									translationDurableStaleAfter,
									translationResultRetention,
								)
								if claimErr != nil {
									retryable := !errors.Is(
										claimErr,
										billing.ErrTranslationRequestConflict,
									)
									result := translateResult{
										seq: job.seq, speaker: job.speaker, original: job.text,
										startTime: job.startTime, endTime: job.endTime,
										err: fmt.Errorf("translation replay lookup failed: %w", claimErr),
									}
									if retryable {
										result.errorType = "translation_processing"
										result.retryAfterMs = int(
											translationProcessingRetry / time.Millisecond,
										)
										result.retryable = true
									}
									if !sendJobResult(result) {
										return
									}
									continue
								}
								switch claim.Disposition {
								case billing.TranslationRequestReplay:
									if !sendJobResult(translateResult{
										seq: job.seq, requestID: job.requestID,
										speaker: job.speaker, content: claim.Result.Content,
										original: job.text, startTime: job.startTime,
										endTime: job.endTime, model: claim.Result.Model,
										latencyMs: claim.Result.LatencyMs,
									}) {
										return
									}
									continue
								case billing.TranslationRequestProcessing:
									if !sendJobResult(translateResult{
										seq: job.seq, speaker: job.speaker, original: job.text,
										startTime: job.startTime, endTime: job.endTime,
										err:          fmt.Errorf("translation request is still processing"),
										errorType:    "translation_processing",
										retryAfterMs: int(translationProcessingRetry / time.Millisecond),
										retryable:    true,
									}) {
										return
									}
									continue
								case billing.TranslationRequestExpired:
									if !sendJobResult(translateResult{
										seq: job.seq, speaker: job.speaker, original: job.text,
										startTime: job.startTime, endTime: job.endTime,
										err: fmt.Errorf(
											"translation replay retention expired; the request was not charged again",
										),
										errorType: "translation_replay_expired",
									}) {
										return
									}
									continue
								case billing.TranslationRequestOwner:
									durableClaim = claim
								default:
									if !sendJobResult(translateResult{
										seq: job.seq, speaker: job.speaker, original: job.text,
										startTime: job.startTime, endTime: job.endTime,
										err: fmt.Errorf("unsupported translation request disposition"),
									}) {
										return
									}
									continue
								}
							}

							cancelDurableClaim := func() {
								if durableClaim == nil || replayBilling == nil {
									return
								}
								cancelCtx, cancelClaim := context.WithTimeout(
									context.Background(),
									5*time.Second,
								)
								defer cancelClaim()
								if cancelErr := replayBilling.CancelTranslationRequest(
									cancelCtx,
									job.requestKey,
									durableClaim.Attempt,
									durableClaim.UsageIdempotencyKey,
								); cancelErr != nil {
									log.Printf("translation request claim cleanup failed: %v", cancelErr)
								}
							}

							translator, prompt, model, runtimeErr := state.translationRuntime()
							if runtimeErr != nil {
								cancelDurableClaim()
								if !sendJobResult(translateResult{
									seq: job.seq, speaker: job.speaker, original: job.text,
									startTime: job.startTime, endTime: job.endTime, err: runtimeErr,
								}) {
									return
								}
								continue
							}

							if meteredProviderFlow {
								if failure, failed := paidFlow.Failure(); failed {
									cancelDurableClaim()
									if !sendJobResult(accountingTranslateResult(&job, failure)) {
										return
									}
									continue
								}
								operationCtx = paidCtx
							}
							if h.billing != nil && userID != "" {
								keyPrefix := "ws-translation:"
								reservationID := ""
								if job.requestKey != "" {
									keyPrefix = ""
									reservationID = job.requestKey
								}
								if durableClaim != nil {
									keyPrefix = ""
									reservationID = durableClaim.UsageIdempotencyKey
								}
								reservation, runtimeErr = reserveRealtimeUsageWithID(
									operationCtx,
									h.billing,
									keyPrefix,
									reservationID,
									&billing.UsageRecord{
										UserID: userID, TenantID: tenantID, SessionID: sessionID,
										Action: "translation", Model: model,
										InputTokens: realtimeInputReservationTokens(
											prompt,
											job.context,
											job.text,
										),
										OutputTokens: realtimeOutputReservationTokens(job.text),
									},
								)
								if runtimeErr != nil {
									duplicateReservation := errors.Is(
										runtimeErr,
										errRealtimeUsageAlreadyRecorded,
									)
									if !duplicateReservation {
										cancelDurableClaim()
									}
									if duplicateReservation && durableClaim != nil {
										if !sendJobResult(translateResult{
											seq: job.seq, speaker: job.speaker, original: job.text,
											startTime: job.startTime, endTime: job.endTime,
											err:       fmt.Errorf("translation request is still processing"),
											errorType: "translation_processing",
											retryAfterMs: int(
												translationProcessingRetry / time.Millisecond,
											),
											retryable: true,
										}) {
											return
										}
										continue
									}
									if duplicateReservation {
										if !sendJobResult(translateResult{
											seq: job.seq, speaker: job.speaker, original: job.text,
											startTime: job.startTime, endTime: job.endTime,
											err: fmt.Errorf(
												"translation request was already processed and cannot be replayed",
											),
										}) {
											return
										}
										continue
									}
									failure := classifyBillingAccountingFailure(runtimeErr)
									log.Printf(
										"translation usage reservation failed: type=%s cause=%v",
										failure.ErrorType,
										runtimeErr,
									)
									if !sendJobResult(accountingTranslateResult(&job, failure)) {
										return
									}
									continue
								}
							}

							translateCtx, translateCancel := context.WithTimeout(
								operationCtx,
								translationProviderTimeout,
							)
							startedAt := time.Now()
							if os.Getenv("OPENAI_DEBUG") == "1" {
								log.Printf("[translate] context_len=%d text_len=%d context_preview=%.200s...", len(job.context), len(job.text), job.context)
							}
							out, usage, translateErr := translator.TranslateWithSystemPromptUsageRetry(
								translateCtx, job.context, job.text, prompt, 3,
							)
							translateCancel()
							if translateErr != nil {
								var refundErr error
								if durableClaim != nil && replayBilling != nil {
									refundCtx, refundCancel := context.WithTimeout(
										context.Background(),
										5*time.Second,
									)
									refundErr = replayBilling.FailTranslationRequest(
										refundCtx,
										job.requestKey,
										durableClaim.Attempt,
										durableClaim.UsageIdempotencyKey,
										"WebSocket translation request failed",
									)
									refundCancel()
								} else {
									refundErr = reservation.refund(
										"WebSocket translation request failed",
									)
								}
								if refundErr != nil {
									desiredFailure := accountingUncertainFailure(refundErr)
									if durableClaim == nil {
										desiredFailure = terminalAccountingUncertainFailure(refundErr)
									}
									failure, first := tripProviderFlow(desiredFailure)
									if durableClaim == nil && failure.Retryable {
										failure = terminalAccountingUncertainFailure(refundErr)
									}
									if !sendJobResult(accountingTranslateResult(&job, failure)) {
										return
									}
									if first {
										closePaidFlowAfterDelivery(failure, job.seq)
									}
									continue
								}
								failed := translateResult{
									seq: job.seq, speaker: job.speaker, original: job.text,
									startTime: job.startTime, endTime: job.endTime, err: translateErr,
								}
								classifyProviderTranslationFailure(
									&failed,
									translateErr,
									refundErr,
								)
								if !sendJobResult(failed) {
									return
								}
								continue
							}

							latency := time.Since(startedAt).Milliseconds()
							inputTokens := max(1, utf8.RuneCountInString(prompt+job.context+job.text)/4)
							outputTokens := max(1, utf8.RuneCountInString(out)/4)
							cachedInputTokens := 0
							cacheWriteTokens := 0
							actualModel := model
							if usage != nil {
								metrics.RecordTranslate(&metrics.Usage{
									PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
									TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens,
									CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model,
								}, latency)
								if os.Getenv("OPENAI_DEBUG") == "1" {
									log.Printf("metrics.translate model=%s tokens p=%d c=%d t=%d latency=%dms",
										usage.Model, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens, latency)
								}
								inputTokens = usage.PromptTokens
								cachedInputTokens = usage.CachedTokens
								cacheWriteTokens = usage.CacheWriteTokens
								outputTokens = usage.CompletionTokens
								actualModel = usage.Model
							} else {
								metrics.RecordTranslateNoUsage(model, latency)
								if os.Getenv("OPENAI_DEBUG") == "1" {
									log.Printf("metrics.translate usage missing; model=%s latency=%dms", model, latency)
								}
							}
							var closeAfterDelivery *websocketAccountingFailure
							if h.billing != nil && userID != "" {
								actualUsage := &billing.UsageRecord{
									UserID: userID, TenantID: tenantID, SessionID: sessionID,
									Action: "translation", Model: actualModel,
									InputTokens: inputTokens, CachedInputTokens: cachedInputTokens,
									CacheWriteTokens: cacheWriteTokens, OutputTokens: outputTokens,
								}
								var cost float64
								var billingErr error
								if durableClaim != nil && replayBilling != nil {
									settleCtx, settleCancel := context.WithTimeout(
										context.Background(),
										10*time.Second,
									)
									cost, billingErr = replayBilling.SettleTranslationRequest(
										settleCtx,
										job.requestKey,
										durableClaim.Attempt,
										durableClaim.UsageIdempotencyKey,
										actualUsage,
										&billing.TranslationReplayResult{
											Content:   strings.TrimSpace(out),
											Model:     actualModel,
											LatencyMs: latency,
										},
										translationResultRetention,
									)
									settleCancel()
								} else {
									cost, billingErr = reservation.settle(actualUsage)
								}
								if billingErr != nil {
									log.Printf("translation usage settlement failed: %v", billingErr)
									if failure, first := tripPaidFlow(billingErr); first {
										closeAfterDelivery = &failure
									}
								} else if cost > 0 {
									if balance, balanceErr := h.billing.GetUserBalance(ctx, userID); balanceErr == nil {
										_ = safeConn.WriteJSON(map[string]interface{}{
											"message": "BalanceUpdated",
											"cost":    cost,
											"balance": balance,
										})
									}
								}
							}

							state.mu.Lock()
							state.recentTranslated = append(state.recentTranslated, strings.TrimSpace(out))
							if len(state.recentTranslated) > state.keepLastTranslated {
								state.recentTranslated = state.recentTranslated[len(state.recentTranslated)-state.keepLastTranslated:]
							}
							state.mu.Unlock()

							if !sendJobResult(translateResult{
								seq: job.seq, speaker: job.speaker, content: strings.TrimSpace(out),
								original: job.text, startTime: job.startTime, endTime: job.endTime,
								model: actualModel, latencyMs: latency,
							}) {
								return
							}
							if closeAfterDelivery != nil {
								closePaidFlowAfterDelivery(*closeAfterDelivery, job.seq)
							}
						}
					}
				}()
			}
		})
	}

	deliveryWG.Add(1)
	go func() {
		defer deliveryWG.Done()
		ordered := newOrderedTranslationResults()
		for result := range results {
			readyResults := ordered.Add(&result)
			for index := range readyResults {
				ready := &readyResults[index]
				if ready.err != nil {
					_ = safeConn.WriteJSON(translationErrorResponse(ready))
					deliveryProgress.Mark(ready.seq)
					continue
				}
				response := serverTranslation{Message: "AddTranslation", Results: []serverTranslationOne{{
					RequestID: ready.requestID,
					Speaker:   ready.speaker, Content: ready.content, Original: ready.original,
					StartTime: ready.startTime, EndTime: ready.endTime,
					Model: ready.model, LatencyMs: ready.latencyMs,
				}}}
				if writeErr := safeConn.WriteJSON(response); writeErr != nil && os.Getenv("OPENAI_DEBUG") == "1" {
					log.Printf("translation delivery write failed: %v", writeErr)
				}
				deliveryProgress.Mark(ready.seq)
			}
		}
	}()

	submitParagraphs := func(paragraphs []pendingParagraph) {
		for _, paragraph := range paragraphs {
			aiActive, contextText := state.translationContext()
			if !aiActive {
				if paragraph.requestID != "" {
					_ = sendResult(translateResult{
						seq:       nextSeq.Add(1),
						requestID: paragraph.requestID,
						err:       fmt.Errorf("AI translation is not active"),
					})
				}
				continue
			}
			state.mu.Lock()
			sessionID := state.sessionID
			state.mu.Unlock()
			sequence := nextSeq.Add(1)
			job := translateJob{
				seq: sequence, requestID: paragraph.requestID,
				requestFingerprint: paragraph.requestFingerprint,
				speaker:            paragraph.speaker, context: contextText,
				text: paragraph.text, startTime: paragraph.startTime,
				endTime: paragraph.endTime, sessionID: sessionID,
				submittedAt: time.Now(),
			}
			if paragraph.requestID != "" {
				cacheKey := translationRequestCacheKey(
					tenantID,
					userID,
					sessionID,
					paragraph.requestID,
				)
				entry, disposition := h.translationRequests.Begin(
					cacheKey,
					paragraph.requestFingerprint,
					time.Now(),
				)
				switch disposition {
				case translationRequestDuplicate:
					replayWG.Add(1)
					go func(seq int64, requestID string, cached *translationRequestEntry) {
						defer replayWG.Done()
						replayed, ok := h.translationRequests.Wait(deliveryWaitCtx, cached)
						if !ok {
							return
						}
						replayed.seq = seq
						replayed.requestID = requestID
						_ = sendResult(replayed)
					}(sequence, paragraph.requestID, entry)
					continue
				case translationRequestConflict:
					_ = sendResult(translateResult{
						seq: sequence, requestID: paragraph.requestID,
						err: fmt.Errorf("request_id was already used for different transcript content"),
					})
					continue
				case translationRequestOverloaded:
					overloaded := translateResult{
						seq: sequence, requestID: paragraph.requestID,
						err: fmt.Errorf("translation idempotency cache is temporarily full"),
					}
					markTranslationProcessing(&overloaded)
					_ = sendResult(overloaded)
					continue
				case translationRequestOwner:
					job.requestCacheKey = cacheKey
					job.requestCacheItem = entry
					job.requestKey = "ws-translation:" + translationReservationID(cacheKey)
				}
			}
			job.operationCtx, job.cancelOperation = context.WithTimeout(
				ctx,
				translationEndToEndBudget,
			)
			timer := time.NewTimer(websocketQueueWait)
			select {
			case jobs <- job:
				timer.Stop()
			case <-ctx.Done():
				timer.Stop()
				job.cancelOperation()
				closed := translateResult{
					requestID: job.requestID,
					err:       fmt.Errorf("translation connection closed before submission"),
				}
				h.translationRequests.Complete(
					job.requestCacheKey,
					job.requestCacheItem,
					&closed,
					time.Now(),
				)
				return
			case <-timer.C:
				job.cancelOperation()
				queueErr := fmt.Errorf("translation work queue remained full")
				failed := translateResult{
					seq: job.seq, requestID: job.requestID, err: queueErr,
				}
				markTranslationProcessing(&failed)
				h.translationRequests.Complete(
					job.requestCacheKey,
					job.requestCacheItem,
					&failed,
					time.Now(),
				)
				_ = sendResult(failed)
				continue
			}
		}
	}

	processRAGParagraphs := func(paragraphs []pendingRAGParagraph) {
		runtime := state.ragRuntime(tenantID, userID)
		for _, paragraph := range paragraphs {
			if meteredProviderFlow {
				if _, failed := paidFlow.Failure(); failed {
					return
				}
			}
			filtered := filterLowInfoText(paragraph.text)
			if strings.TrimSpace(filtered) == "" {
				continue
			}
			if runtime.service != nil {
				runtime.service.RecordLiveParagraph(
					runtime.sessionID, paragraph.speaker, paragraph.text, filtered,
					paragraph.startTime, paragraph.endTime,
				)
				service := runtime.service
				sessionID := runtime.sessionID
				item := paragraph
				text := filtered
				rawSessionID, _ := state.sessionSnapshot()
				billingSessionID := billingSessionReference(rawSessionID)
				enqueueAuxiliary(func() {
					operationCtx := ctx
					if meteredProviderFlow {
						if _, failed := paidFlow.Failure(); failed {
							return
						}
						operationCtx = paidCtx
						operationCtx = rag.WithProviderUsageMeter(
							operationCtx,
							&websocketRAGUsageMeter{
								billing:        h.billing,
								tenantID:       tenantID,
								userID:         userID,
								sessionID:      billingSessionID,
								onBillingError: failClosePaidFlow,
							},
						)
					}
					if ingestErr := service.IngestParagraph(
						operationCtx,
						sessionID,
						item.speaker,
						text,
						item.startTime,
						item.endTime,
					); ingestErr != nil && operationCtx.Err() == nil {
						log.Printf("rag ingest error: %v", ingestErr)
					}
				})
			}
			if runtime.shouldUpdateSummary {
				text := filtered
				enqueueAuxiliary(func() {
					operationCtx := ctx
					if meteredProviderFlow {
						if _, failed := paidFlow.Failure(); failed {
							return
						}
						operationCtx = paidCtx
					}
					if summaryErr := state.updateSummaryIncremental(
						operationCtx,
						text,
						h.billing,
						userID,
						tenantID,
						failClosePaidFlow,
					); summaryErr != nil && operationCtx.Err() == nil {
						if failure, ok := websocketAccountingFailureFromError(summaryErr); ok {
							_ = safeConn.WriteJSON(failure.response())
						} else {
							_ = safeConn.WriteJSON(map[string]interface{}{
								"message":   "Error",
								"type":      "summary_error",
								"reason":    summaryErr.Error(),
								"retryable": false,
							})
						}
					}
				})
			}
		}
	}

	var flushMu sync.Mutex
	flushBuffersLocked := func(force bool) {
		paragraphs, ragParagraphs := state.flushPending(time.Now(), force)
		submitParagraphs(paragraphs)
		processRAGParagraphs(ragParagraphs)
	}
	flushBuffers := func(force bool) {
		flushMu.Lock()
		defer flushMu.Unlock()
		flushBuffersLocked(force)
	}

	flushStop := make(chan struct{})
	var flushWG sync.WaitGroup
	flushWG.Add(1)
	go func() {
		defer flushWG.Done()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-flushStop:
				return
			case <-ticker.C:
				flushBuffers(false)
			}
		}
	}()

	waitForDelivery := func(waitCtx context.Context, target int64) bool {
		if target <= 0 {
			return true
		}
		return deliveryProgress.Wait(waitCtx, target)
	}

	// Read loop
readLoop:
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			log.Printf("WebSocket connection closed from %s", strconv.Quote(r.RemoteAddr))
			break readLoop
		}
		if err := conn.SetReadDeadline(time.Now().Add(translationPongWait)); err != nil {
			log.Printf("WebSocket read deadline refresh failed: %v", err)
			break readLoop
		}

		if messageType == websocket.BinaryMessage {
			// Not used
			continue
		}

		var cli clientMessage
		if err := json.Unmarshal(message, &cli); err != nil {
			log.Printf("WS: invalid JSON: %v", err)
			_ = safeConn.WriteJSON(map[string]string{
				"message": "Error",
				"reason":  "invalid JSON message",
			})
			continue
		}
		if validationErr := validateClientMessage(&cli); validationErr != nil {
			_ = safeConn.WriteJSON(map[string]string{
				"message": "Error",
				"reason":  validationErr.Error(),
			})
			continue
		}
		configAdjusted := false
		if meteredProviderFlow &&
			strings.EqualFold(strings.TrimSpace(cli.Type), "init") {
			cli.Config, configAdjusted = sanitizeMeteredClientConfig(cli.Config)
		}

		switch strings.ToLower(strings.TrimSpace(cli.Type)) {
		case "init":
			currentSessionID, alreadyInited := state.sessionSnapshot()
			requestedSessionID := ""
			if cli.Config != nil {
				requestedSessionID = strings.TrimSpace(cli.Config.SessionID)
			}
			if alreadyInited && requestedSessionID != "" && requestedSessionID != currentSessionID {
				// Submit everything under the old namespace, then wait for both
				// translation delivery and RAG/summary persistence before
				// clearing context and accepting the new session id.
				flushMu.Lock()
				flushBuffersLocked(true)
				translationTarget := nextSeq.Load()
				auxiliaryTarget := auxiliaryNext.Load()
				flushMu.Unlock()

				barrierCtx, barrierCancel := context.WithTimeout(
					ctx,
					translationBarrierTimeout,
				)
				delivered := waitForDelivery(barrierCtx, translationTarget)
				persisted := delivered && auxiliaryProgress.Wait(barrierCtx, auxiliaryTarget)
				barrierCancel()
				if !persisted {
					_ = safeConn.WriteJSON(map[string]string{
						"message": "Error",
						"reason":  "session switch timed out while draining the previous session",
					})
					break readLoop
				}
				state.resetSessionContext(requestedSessionID)
			}
			if cli.Mode != nil {
				state.setMode(*cli.Mode)
			}
			state.applyConfig(cli.Config)
			// If we have a RAG service and a summary model override, enforce it via custom provider
			if state.ragSvc != nil {
				state.mu.Lock()
				model := state.selectedModelSummary
				state.mu.Unlock()
				state.ragSvc.SetChatConfigProvider(func() (*openai.Config, error) {
					if strings.TrimSpace(model) == "" {
						return nil, errors.New("approved summary model is unavailable")
					}
					return aiproviders.ConfigFor(model)
				})
			}
			state.mu.Lock()
			state.inited = true
			state.mu.Unlock()
			// Worker count is taken from the first init, after the client's
			// bounded configuration has been applied.
			startWorkers(state.workerCount())
			if configAdjusted {
				_ = safeConn.WriteJSON(map[string]string{
					"message": "Info",
					"type":    "config_adjusted",
					"reason":  "client model overrides ignored; using server-managed models",
				})
			}
			// Acknowledge
			_ = safeConn.WriteJSON(map[string]interface{}{
				"message": "Info",
				"reason":  "translator initialized",
				"workers": activeWorkers,
				"capabilities": map[string]bool{
					"request_ids":        true,
					"atomic_transcripts": true,
					"async_flush":        true,
				},
			})

		case "transcript":
			if cli.Payload == nil {
				continue
			}
			seg := strings.TrimSpace(cli.Payload.Transcript)
			if seg == "" {
				continue
			}
			// Require init before processing transcripts
			state.mu.Lock()
			inited := state.inited
			state.mu.Unlock()
			if !inited {
				continue
			}
			if !state.acceptSpeaker(cli.Payload.Speaker) {
				_ = safeConn.WriteJSON(map[string]string{
					"message": "Error",
					"reason":  "too many distinct speakers on one connection",
				})
				continue
			}
			// Maintain state with original EN segment
			state.addSegmentEN(seg)
			// Possibly trigger async compression
			// Compressed模式不再按大块重压缩，改为在段落flush时做增量摘要，节省tokens

			// RAG live ingestion runs on its own buffers so translation batching can stay conservative.
			ragRuntime := state.ragRuntime(tenantID, userID)
			if ragRuntime.service != nil || ragRuntime.summarizationEnabled {
				if flushed, text, start, end := state.handleRAGAggregation(
					cli.Payload.Speaker, seg, cli.Payload.StartTime, cli.Payload.EndTime,
				); flushed {
					processRAGParagraphs([]pendingRAGParagraph{{
						speaker: cli.Payload.Speaker, text: text, startTime: start, endTime: end,
					}})
				}
			}

			// ID-capable clients have already aligned provider micro-finals into
			// sentence/card chunks. Treat each payload as one atomic paragraph:
			// this removes the per-chunk flush barrier while preserving the
			// legacy server-side aggregation path for clients without IDs.
			if cli.Payload.RequestID != "" {
				submitParagraphs([]pendingParagraph{{
					requestID:          cli.Payload.RequestID,
					requestFingerprint: translationRequestFingerprint(cli.Payload),
					speaker:            cli.Payload.Speaker,
					text:               seg,
					startTime:          cli.Payload.StartTime,
					endTime:            cli.Payload.EndTime,
				}})
				continue
			}

			// Append to aggregator and flush sentences -> batch into paragraphs
			if flushed, sentText, sSent, eSent := state.handleAggregation(cli.Payload.Speaker, seg, cli.Payload.StartTime, cli.Payload.EndTime); flushed {
				if doFlush, paraText, sPara, ePara := state.enqueueSentence(cli.Payload.Speaker, sentText, sSent, eSent); doFlush {
					submitParagraphs([]pendingParagraph{{
						speaker: cli.Payload.Speaker, text: paraText, startTime: sPara, endTime: ePara,
					}})
				}
			}
		case "flush", "stop", "end", "end_of_stream":
			flushMu.Lock()
			flushBuffersLocked(true)
			translationTarget := nextSeq.Load()
			flushMu.Unlock()

			// Waiting for provider work must not block the WebSocket read loop:
			// pings, reconnect coordination, and subsequent legacy transcripts
			// continue to be accepted while this ordered barrier completes.
			select {
			case flushBarrierSlots <- struct{}{}:
			default:
				_ = safeConn.WriteJSON(map[string]string{
					"message": "Info",
					"type":    "flush_coalesced",
					"reason":  "flush work was accepted; an earlier barrier is still pending",
				})
				continue
			}
			barrierWG.Add(1)
			go func(target int64) {
				defer barrierWG.Done()
				defer func() { <-flushBarrierSlots }()
				barrierCtx, barrierCancel := context.WithTimeout(
					deliveryWaitCtx,
					translationBarrierTimeout,
				)
				delivered := waitForDelivery(barrierCtx, target)
				barrierCancel()
				if delivered {
					_ = safeConn.WriteJSON(map[string]interface{}{
						"message": "Info",
						"reason":  "pending buffers flushed",
						"seq":     target,
					})
					return
				}
				if deliveryWaitCtx.Err() == nil {
					_ = safeConn.WriteJSON(map[string]string{
						"message": "Info",
						"type":    "flush_timeout",
						"reason":  "flush timed out before translations were delivered",
					})
				}
			}(translationTarget)
		case "ping":
			_ = safeConn.WriteJSON(map[string]interface{}{
				"message": "Pong",
				"ts":      time.Now().UnixMilli(),
			})
		default:
			// ignore
		}
	}

	stopDeliveryWait()
	stopHeartbeat()

	// Stop the timer before a final forced drain so no producer can race with
	// channel closure. Let queued translations finish (their own timeout is
	// bounded), then close the delivery stream in order.
	close(flushStop)
	flushTimerDone := make(chan struct{})
	go func() {
		flushWG.Wait()
		close(flushTimerDone)
	}()
	select {
	case <-flushTimerDone:
	case <-time.After(2 * time.Second):
		// The timer may already be blocked applying backpressure to a full
		// provider queue. Cancellation makes every queue send abandon promptly.
		cancel()
		<-flushTimerDone
	}

	finalFlushDone := make(chan struct{})
	go func() {
		flushBuffers(true)
		close(finalFlushDone)
	}()
	select {
	case <-finalFlushDone:
	case <-time.After(2 * time.Second):
		cancel()
		<-finalFlushDone
	}
	close(jobs)
	workersDone := make(chan struct{})
	go func() {
		workerWG.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
	case <-time.After(translationBarrierTimeout):
		cancel()
		<-workersDone
	}
	replayWG.Wait()
	close(results)
	deliveryWG.Wait()
	barrierWG.Wait()

	// Give final RAG persistence a short grace period, then explicitly cancel
	// all remaining work and wait for it to observe cancellation before the RAG
	// service is closed.
	close(auxiliaryTasks)
	auxiliaryDone := make(chan struct{})
	go func() {
		auxiliaryWG.Wait()
		close(auxiliaryDone)
	}()
	select {
	case <-auxiliaryDone:
	case <-time.After(3 * time.Second):
		cancel()
		<-auxiliaryDone
	}
	cancel()
}
