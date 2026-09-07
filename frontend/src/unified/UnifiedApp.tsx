import { useState } from 'react'
import { useMessages } from '../i18n'
import { AuthGate } from './components/AuthGate'
import { BrandMark } from './components/BrandMark'
import { useUnifiedAuth } from './hooks/useUnifiedAuth'
import { useUnifiedSettings, type UnifiedSettings } from './hooks/useUnifiedSettings'
import { useUnifiedWorkspace } from './hooks/useUnifiedWorkspace'
import { WorkspaceShell } from './WorkspaceShell'
import './UnifiedApp.css'

interface UnifiedAppProps {
  proEntry?: boolean
}

export default function UnifiedApp({ proEntry: explicitProEntry }: UnifiedAppProps) {
  const proEntry = explicitProEntry ?? (
    window.location.pathname === '/pro'
    || window.location.pathname === '/pro.html'
    || window.location.pathname.startsWith('/pro/')
  )
  const m = useMessages()
  const auth = useUnifiedAuth()
  const { settings, patchSettings } = useUnifiedSettings()
  const [authRequested, setAuthRequested] = useState(false)
  const workspace = useUnifiedWorkspace({
    ragEnabled: auth.ragEnabled,
    settings,
    user: auth.user,
    onBalanceUpdated: auth.applyBalance,
  })

  const changeSettings = (patch: Partial<UnifiedSettings>) => {
    patchSettings({
      ...patch,
      ...(patch.translationEnabled === false ? { viewMode: 'original' as const } : {}),
    })
  }

  if (auth.checking) {
    return (
      <main className="dt-app-loading">
        <div>
          <BrandMark className="dt-brand__mark" />
          <span>{m.common.opening}</span>
        </div>
      </main>
    )
  }

  const requiresGate = workspace.recorderStatus === 'idle'
    && !auth.user
    && (
      proEntry
      || !auth.anonymousAllowed
      || authRequested
    )

  if (requiresGate) {
    return (
      <AuthGate
        allowAnonymous={auth.anonymousAllowed}
        emailVerificationRequired={auth.emailVerificationRequired}
        error={auth.error}
        pendingVerification={auth.pendingVerification}
        proEntry={proEntry}
        registrationEnabled={auth.registrationEnabled}
        submitting={auth.submitting}
        verificationOutcome={auth.verificationOutcome}
        onContinueAnonymous={() => {
          auth.clearError()
          setAuthRequested(false)
        }}
        onDismissVerification={auth.dismissVerification}
        onResendVerification={auth.resendVerification}
        onLogin={async (email, password) => {
          const successful = await auth.login(email, password)
          if (successful) setAuthRequested(false)
          return successful
        }}
        onRegister={async (input) => {
          const successful = await auth.register(input)
          if (successful) setAuthRequested(false)
          return successful
        }}
      />
    )
  }

  return (
    <WorkspaceShell
      allowUserApiKey={auth.allowUserApiKey}
      account={auth.account}
      balance={auth.balance}
      paymentsEnabled={auth.paymentsEnabled}
      connectionLabel={workspace.connectionLabel}
      durationLabel={workspace.durationLabel}
      error={workspace.error}
      transportDiagnostics={workspace.transportDiagnostics}
      feedGeneration={workspace.feedGeneration}
      feedItems={workspace.feedItems}
      historyLoading={workspace.historyLoading}
      historyOpening={workspace.historyOpening}
      historySessions={workspace.historySessions}
      legacyHistoryCount={workspace.legacyHistoryCount}
      pendingWrites={workspace.pendingWrites}
      paymentRequired={workspace.paymentRequired}
      ragEnabled={auth.ragEnabled}
      recorderStatus={workspace.recorderStatus}
      sessionCost={workspace.sessionCost}
      sessionId={workspace.sessionId}
      sessionSourceLanguage={workspace.sessionSourceLanguage}
      settings={settings}
      stats={workspace.stats}
      title={workspace.title}
      titleGenerating={workspace.titleGenerating}
      transcriptContext={workspace.transcriptContext}
      trainingProgram={auth.trainingProgram}
      user={auth.user}
      onClearError={workspace.clearError}
      onContinue={workspace.continueSession}
      onDeleteHistory={workspace.deleteHistory}
      onEndHistorySession={workspace.endHistorySession}
      onUploadHistorySessionToCloud={workspace.uploadHistorySessionToCloud}
      onDownloadAudio={workspace.downloadAudio}
      onDownloadText={workspace.downloadText}
      onLoadHistory={workspace.loadHistory}
      onMigrateLegacyHistory={workspace.migrateLegacyHistory}
      onLogout={async () => {
        await workspace.stop()
        await auth.logout()
      }}
      onPauseToggle={workspace.pauseToggle}
      onRefreshAccount={auth.refreshAccount}
      onRefreshHistory={workspace.refreshHistory}
      onRequestLogin={() => {
        auth.clearError()
        void workspace.stop().finally(() => setAuthRequested(true))
      }}
      onSettingsChange={changeSettings}
      onStart={workspace.start}
      onStop={workspace.stop}
      onTitleChange={workspace.updateTitle}
      onGenerateTitle={workspace.generateTitle}
      onTrainingOptInChange={auth.setTrainingOptIn}
    />
  )
}
