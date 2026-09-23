import { checkSpeechmaticsPreflight } from '../../pro/api/auth'
import { messages } from '../../i18n'

function errorMessage(reason: unknown): string {
  if (reason instanceof Error) return reason.message.trim()
  return String(reason ?? '').trim()
}

export function speechmaticsPreflightErrorMessage(reason: unknown): string {
  const copy = messages().workspace.runtime.preflight
  const message = errorMessage(reason)
  const normalized = message.toLowerCase()

  if (
    normalized.includes('insufficient balance')
    || normalized.includes('balance is insufficient')
  ) {
    return copy.balance
  }
  if (normalized.includes('websocket origin not allowed')) {
    return copy.origin
  }
  if (
    normalized.includes('transcription quota exceeded')
    || normalized.includes('monthly api quota exceeded')
  ) {
    return copy.quota
  }
  if (
    normalized.includes('session expired')
    || normalized.includes('not authenticated')
    || normalized.includes('authentication state changed')
    || normalized.includes('authentication required')
    || normalized.includes('invalid or expired access token')
    || normalized.includes('invalid credentials')
  ) {
    return copy.auth
  }
  if (normalized.includes('concurrent transcription limit reached')) {
    return copy.concurrent
  }
  if (
    normalized.includes('rate limit exceeded')
    || normalized.includes('too many active websocket connections')
  ) {
    return copy.rateLimit
  }
  if (
    normalized.includes('service unavailable')
    || normalized.includes('temporarily unavailable')
    || normalized.includes('preflight returned an invalid response')
  ) {
    return copy.unavailable
  }

  return message
    ? copy.failed(message)
    : copy.failedGeneric
}

export async function ensureSpeechmaticsPreflight(mainTransport = false): Promise<void> {
  try {
    const clientOrigin = typeof window === 'undefined' ? '' : window.location.origin
    await checkSpeechmaticsPreflight(clientOrigin, mainTransport)
  } catch (reason) {
    throw new Error(speechmaticsPreflightErrorMessage(reason), {
      cause: reason,
    })
  }
}
