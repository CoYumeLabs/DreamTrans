// Package speechmatics provides bounded real-time and batch API clients.
package speechmatics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

const (
	batchAPIBaseURL = "https://asr.api.speechmatics.com/v2"
	defaultTimeout  = 30 * time.Second
	maxBatchAudio   = 100 << 20
	maxBatchWait    = 24 * time.Hour
)

// ErrBatchSubmissionUncertain means the request may have reached the provider,
// but no trustworthy job identifier was returned. Callers must not
// automatically refund a cost reservation in this state.
var ErrBatchSubmissionUncertain = errors.New("batch submission outcome is uncertain")

// BatchClient handles interactions with Speechmatics Batch API
type BatchClient struct {
	apiKey     string
	httpClient *http.Client
}

// NewBatchClient creates a new Speechmatics Batch API client
func NewBatchClient(apiKey string) *BatchClient {
	return &BatchClient{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: defaultTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// TranscriptionConfig represents the transcription configuration
type TranscriptionConfig struct {
	Language       string  `json:"language"`
	Diarization    string  `json:"diarization,omitempty"`
	EnablePartials bool    `json:"-"`
	OperatingPoint string  `json:"operating_point,omitempty"`
	MaxDelay       float64 `json:"-"`
}

// JobConfig represents the job configuration
type JobConfig struct {
	Type                string              `json:"type"`
	Reference           string              `json:"-"`
	TranscriptionConfig TranscriptionConfig `json:"transcription_config"`
}

// MarshalJSON keeps the public helper's reference while using the provider's
// tracking object. Realtime-only partial/delay options are never sent to batch.
func (c *JobConfig) MarshalJSON() ([]byte, error) {
	type wireConfig JobConfig
	var tracking *struct {
		Reference string `json:"reference"`
	}
	if c.Reference != "" {
		tracking = &struct {
			Reference string `json:"reference"`
		}{c.Reference}
	}
	return json.Marshal(struct {
		wireConfig
		Tracking *struct {
			Reference string `json:"reference"`
		} `json:"tracking,omitempty"`
	}{wireConfig: wireConfig(*c), Tracking: tracking})
}

// JobResponse represents the response from job submission
type JobResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// UnmarshalJSON accepts the job envelope returned by GET /jobs/{id}, as
// well as the flat ID returned by POST /jobs and older compatible providers.
func (j *JobResponse) UnmarshalJSON(data []byte) error {
	type jobFields JobResponse
	var wire struct {
		jobFields
		Job *jobFields `json:"job"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Job != nil {
		*j = JobResponse(*wire.Job)
	} else {
		*j = JobResponse(wire.jobFields)
	}
	return nil
}

// TranscriptResponse represents the transcript retrieval response
type TranscriptResponse struct {
	Job *struct {
		ID       string   `json:"id"`
		Duration *float64 `json:"duration"`
	} `json:"job,omitempty"`
	Format   string `json:"format"`
	Content  string `json:"content"`
	Metadata struct {
		CreatedAt string  `json:"created_at"`
		Duration  float64 `json:"duration"`
		Language  string  `json:"language"`
	} `json:"metadata"`
	Results []TranscriptResult `json:"results"`
}

// TranscriptResult represents a single transcript segment
type TranscriptResult struct {
	Alternatives []struct {
		Content    string  `json:"content"`
		Confidence float64 `json:"confidence"`
		Speaker    string  `json:"speaker,omitempty"`
	} `json:"alternatives"`
	StartTime float64 `json:"start_time"`
	EndTime   float64 `json:"end_time"`
	Type      string  `json:"type"`
}

// SubmitJob submits an audio file for transcription
func (c *BatchClient) SubmitJob(audioData []byte, filename string, config *JobConfig) (*JobResponse, error) {
	return c.SubmitJobContext(context.Background(), audioData, filename, config)
}

// SubmitJobContext submits an audio file and observes caller cancellation.
func (c *BatchClient) SubmitJobContext(ctx context.Context, audioData []byte, filename string, config *JobConfig) (*JobResponse, error) {
	if len(audioData) == 0 || len(audioData) > maxBatchAudio {
		return nil, fmt.Errorf("audio data must be between 1 byte and 100 MiB")
	}
	return c.SubmitJobReaderContext(ctx, bytes.NewReader(audioData), int64(len(audioData)), filename, config)
}

// SubmitJobReaderContext streams multipart audio directly to Speechmatics.
// This avoids retaining a second 100 MiB copy of an already uploaded file.
//
//nolint:gocyclo // Multipart streaming requires explicit cleanup at each request boundary.
func (c *BatchClient) SubmitJobReaderContext(
	ctx context.Context,
	audio io.Reader,
	audioSize int64,
	filename string,
	config *JobConfig,
) (*JobResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context must not be nil")
	}
	if err := c.validateCredentials(); err != nil {
		return nil, err
	}
	if audio == nil || audioSize <= 0 || audioSize > maxBatchAudio {
		return nil, fmt.Errorf("audio data must be between 1 byte and 100 MiB")
	}
	if err := validateJobConfig(config); err != nil {
		return nil, err
	}
	filename = filepath.Base(strings.ReplaceAll(strings.TrimSpace(filename), `\`, "/"))
	filename = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, filename)
	if filename == "." || filename == "" {
		filename = "audio"
	}
	if runes := []rune(filename); len(runes) > 200 {
		filename = string(runes[:200])
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config: %w", err)
	}
	bodyReader, bodyWriter := io.Pipe()
	defer func() { _ = bodyReader.Close() }()
	writer := multipart.NewWriter(bodyWriter)
	contentType := writer.FormDataContentType()
	writeDone := make(chan error, 1)
	go func() {
		var writeErr error
		part, createErr := writer.CreateFormFile("data_file", filename)
		if createErr != nil {
			writeErr = fmt.Errorf("failed to create form file: %w", createErr)
		} else {
			written, copyErr := io.Copy(part, io.LimitReader(audio, audioSize+1))
			if copyErr != nil {
				writeErr = fmt.Errorf("failed to stream audio data: %w", copyErr)
			} else if written != audioSize {
				writeErr = fmt.Errorf("audio size changed while streaming")
			}
		}
		if writeErr == nil {
			if fieldErr := writer.WriteField("config", string(configJSON)); fieldErr != nil {
				writeErr = fmt.Errorf("failed to write config field: %w", fieldErr)
			}
		}
		if closeErr := writer.Close(); writeErr == nil && closeErr != nil {
			writeErr = fmt.Errorf("failed to close multipart writer: %w", closeErr)
		}
		_ = bodyWriter.CloseWithError(writeErr)
		writeDone <- writeErr
	}()

	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/jobs/", batchAPIBaseURL), bodyReader)
	if err != nil {
		_ = bodyReader.CloseWithError(err)
		<-writeDone
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", contentType)

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		_ = bodyReader.CloseWithError(err)
		<-writeDone
		return nil, fmt.Errorf("%w: failed to send request: %v", ErrBatchSubmissionUncertain, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if writeErr := <-writeDone; writeErr != nil {
		return nil, fmt.Errorf("%w: %v", ErrBatchSubmissionUncertain, writeErr)
	}

	// Read response
	respBody, err := readBatchResponse(resp.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read response: %v", ErrBatchSubmissionUncertain, err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= http.StatusInternalServerError ||
			resp.StatusCode == http.StatusRequestTimeout ||
			resp.StatusCode == http.StatusTooEarly {
			return nil, fmt.Errorf(
				"%w: speechmatics API error (status %d)",
				ErrBatchSubmissionUncertain,
				resp.StatusCode,
			)
		}
		return nil, fmt.Errorf("speechmatics API error (status %d)", resp.StatusCode)
	}

	// Parse response
	var jobResp JobResponse
	if err := json.Unmarshal(respBody, &jobResp); err != nil {
		return nil, fmt.Errorf("%w: failed to parse response: %v", ErrBatchSubmissionUncertain, err)
	}
	if !validJobID(jobResp.ID) {
		return nil, fmt.Errorf("%w: upstream returned an invalid job id", ErrBatchSubmissionUncertain)
	}
	if !validProtocolValue(jobResp.Status, 50, true) {
		return nil, fmt.Errorf("upstream returned an invalid job status")
	}

	return &jobResp, nil
}

// GetJobStatus retrieves the status of a transcription job
func (c *BatchClient) GetJobStatus(jobID string) (*JobResponse, error) {
	return c.GetJobStatusContext(context.Background(), jobID)
}

func (c *BatchClient) GetJobStatusContext(ctx context.Context, jobID string) (*JobResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context must not be nil")
	}
	if err := c.validateCredentials(); err != nil {
		return nil, err
	}
	if !validJobID(jobID) {
		return nil, fmt.Errorf("invalid job id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/jobs/%s", batchAPIBaseURL, jobID), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = readBatchResponse(resp.Body, 1<<20)
		return nil, fmt.Errorf("speechmatics API error (status %d)", resp.StatusCode)
	}

	var jobResp JobResponse
	body, err := readBatchResponse(resp.Body, 1<<20)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &jobResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	if !validJobID(jobResp.ID) || !validProtocolValue(jobResp.Status, 50, false) {
		return nil, fmt.Errorf("upstream returned an invalid job response")
	}

	return &jobResp, nil
}

// DeleteJobContext force-cancels a running batch job and removes its provider
// resources. It is used when local ownership persistence fails after the
// provider has already accepted the upload.
func (c *BatchClient) DeleteJobContext(ctx context.Context, jobID string) error {
	if ctx == nil {
		return fmt.Errorf("context must not be nil")
	}
	if err := c.validateCredentials(); err != nil {
		return err
	}
	if !validJobID(jobID) {
		return fmt.Errorf("invalid job id")
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		fmt.Sprintf("%s/jobs/%s?force=true", batchAPIBaseURL, jobID),
		http.NoBody,
	)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := readBatchResponse(resp.Body, 1<<20); err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("speechmatics API error (status %d)", resp.StatusCode)
	}
	return nil
}

// GetTranscript retrieves the transcript for a completed job
func (c *BatchClient) GetTranscript(jobID, format string) (*TranscriptResponse, error) {
	return c.GetTranscriptContext(context.Background(), jobID, format)
}

func (c *BatchClient) GetTranscriptContext(ctx context.Context, jobID, format string) (*TranscriptResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context must not be nil")
	}
	if err := c.validateCredentials(); err != nil {
		return nil, err
	}
	if !validJobID(jobID) {
		return nil, fmt.Errorf("invalid job id")
	}
	if format == "" {
		format = "json-v2"
	}
	if format != "json-v2" && format != "txt" {
		return nil, fmt.Errorf("unsupported transcript format")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/jobs/%s/transcript?format=%s", batchAPIBaseURL, jobID, format), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = readBatchResponse(resp.Body, 1<<20)
		return nil, fmt.Errorf("speechmatics API error (status %d)", resp.StatusCode)
	}

	respBody, err := readBatchResponse(resp.Body, 64<<20)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// If format is txt, return raw text
	if format == "txt" {
		return &TranscriptResponse{
			Format:  format,
			Content: string(respBody),
		}, nil
	}

	// Otherwise parse JSON
	var transcriptResp TranscriptResponse
	if err := json.Unmarshal(respBody, &transcriptResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// json-v2 reports billable file duration under job, not recognition metadata.
	if transcriptResp.Job != nil && transcriptResp.Job.Duration != nil {
		if transcriptResp.Job.ID != "" && transcriptResp.Job.ID != jobID {
			return nil, fmt.Errorf("transcript job id mismatch")
		}
		transcriptResp.Metadata.Duration = *transcriptResp.Job.Duration
	} else if transcriptResp.Metadata.Duration <= 0 {
		return nil, fmt.Errorf("transcript is missing the billable audio duration")
	}
	if math.IsNaN(transcriptResp.Metadata.Duration) || math.IsInf(transcriptResp.Metadata.Duration, 0) || transcriptResp.Metadata.Duration < 0 {
		return nil, fmt.Errorf("transcript has invalid audio duration")
	}
	return &transcriptResp, nil
}

// WaitForCompletion polls the job status until it's completed or failed
func (c *BatchClient) WaitForCompletion(jobID string, maxWaitTime time.Duration) error {
	return c.WaitForCompletionContext(context.Background(), jobID, maxWaitTime)
}

func (c *BatchClient) WaitForCompletionContext(ctx context.Context, jobID string, maxWaitTime time.Duration) error {
	if ctx == nil {
		return fmt.Errorf("context must not be nil")
	}
	if !validJobID(jobID) {
		return fmt.Errorf("invalid job id")
	}
	if maxWaitTime <= 0 || maxWaitTime > maxBatchWait {
		return fmt.Errorf("max wait time must be between 1 nanosecond and 24 hours")
	}
	waitCtx, cancel := context.WithTimeout(ctx, maxWaitTime)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		status, err := c.GetJobStatusContext(waitCtx, jobID)
		if err != nil {
			if waitCtx.Err() != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("timeout waiting for job completion")
			}
			return fmt.Errorf("failed to get job status: %w", err)
		}

		switch status.Status {
		case "done":
			return nil
		case "rejected", "deleted", "error":
			return fmt.Errorf("job failed with status: %s", status.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-waitCtx.Done():
			return fmt.Errorf("timeout waiting for job completion")
		case <-ticker.C:
		}
	}
}

func (c *BatchClient) validateCredentials() error {
	if strings.TrimSpace(c.apiKey) == "" || len(c.apiKey) > 4096 {
		return fmt.Errorf("invalid Speechmatics API key configuration")
	}
	for _, character := range c.apiKey {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("invalid Speechmatics API key configuration")
		}
	}
	return nil
}

func validateJobConfig(config *JobConfig) error {
	if config == nil || config.Type != "transcription" {
		return fmt.Errorf("invalid job configuration")
	}
	transcription := config.TranscriptionConfig
	if !validProviderReference(config.Reference) ||
		!validProtocolValue(transcription.Language, 10, false) ||
		!validProtocolValue(transcription.Diarization, 50, true) ||
		!validProtocolValue(transcription.OperatingPoint, 50, true) ||
		math.IsNaN(transcription.MaxDelay) ||
		math.IsInf(transcription.MaxDelay, 0) ||
		transcription.MaxDelay < 0 ||
		transcription.MaxDelay > 30 {
		return fmt.Errorf("invalid job configuration")
	}
	return nil
}

func validProviderReference(reference string) bool {
	if len(reference) > 200 {
		return false
	}
	for _, character := range reference {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func validProtocolValue(value string, maxLength int, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) > maxLength || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validJobID(jobID string) bool {
	if jobID == "" || len(jobID) > 200 {
		return false
	}
	for _, char := range jobID {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func readBatchResponse(reader io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, fmt.Errorf("speechmatics response exceeds %d bytes", limit)
	}
	return payload, nil
}
