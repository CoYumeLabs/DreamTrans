> 历史归档：本文描述旧版本，不代表当前实现。当前使用方式见 [用户指南](USER_GUIDE.md)，功能审计见 [2026-09-07 审计](audits/product-consistency-2026-09-07.md)。

# Project Status: Real-Time Transcription App

## 1. Project Goal

To build a real-time classroom transcription and translation application. The current focus is on perfecting the core transcription functionality.

## 2. Architecture

We have adopted a **Backend-for-Frontend (BFF)** architecture for optimal performance and security:

*   **Frontend (React + TypeScript):**
    *   Handles all user interface elements.
    *   Captures microphone audio using the Web Audio API.
    *   Connects **directly** to the Speechmatics Real-Time API via WebSockets using the official SDK.
    *   Responsible for rendering the incoming transcription.

*   **Backend (Go):**
    *   **/api/token/rt:** A secure REST endpoint that generates short-lived JSON Web Tokens (JWTs). This prevents the main API key from being exposed in the frontend.
    *   **/ws/translate:** A WebSocket endpoint stub, planned for future translation features. It will receive finalized transcripts from the frontend.

## 3. Implementation Status & Milestones

### Backend (Go) - ✅ Complete & Stable
*   The JWT generation service is fully functional and correctly configured with all required claims for the Speechmatics API.
*   The WebSocket endpoint for translation is in place.
*   The backend requires no further work for the current transcription task.

### Frontend (React) - 🚧 In Progress
*   **Milestone:** Successfully integrated the Speechmatics SDK and established an end-to-end connection.
*   **Milestone:** Systematically debugged and resolved a series of critical connection errors (`AudioContext`, `not_authorised`, `protocol_error`, `invalid_audio_type`).
*   **Achievement:** **Live transcription is working!** We can speak into the microphone and see the text appear on the screen.

## 4. Current Status & Next Steps

*   **Current Status:** The core functionality is proven and operational. However, the default word-by-word display from the SDK does not provide an ideal user experience.

*   **Next Step:** Refactor the frontend display logic to create a more natural "typing effect."
    *   **Plan:** We will manage the transcript using two states: `finalizedLines` for completed sentences and `partialLine` for the sentence currently being transcribed. This will ensure that temporary results are smoothly replaced by final, more accurate results, providing a much cleaner and more readable user interface.