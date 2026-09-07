import InviteLanding from './landing/InviteLanding'
import LandingPage from './landing/LandingPage'
import LegalPage from './legal/LegalPage'
import type { LegalKind } from './legal/documents'
import UnifiedApp from './unified/UnifiedApp'

function normalizePath(pathname: string): string {
  if (pathname.length > 1 && pathname.endsWith('/')) {
    return pathname.slice(0, -1)
  }
  return pathname || '/'
}

function legalKind(pathname: string): LegalKind | null {
  const path = normalizePath(pathname)
  if (path === '/privacy' || path === '/privacy.html') return 'privacy'
  if (
    path === '/terms'
    || path === '/terms.html'
    || path === '/tos'
    || path === '/terms-of-service'
  ) {
    return 'terms'
  }
  return null
}

function shouldOpenWorkspace(pathname: string, search: string): boolean {
  if (
    pathname === '/pro'
    || pathname === '/pro.html'
    || pathname.startsWith('/pro/')
  ) {
    return true
  }
  if (pathname === '/app' || pathname === '/workspace') {
    return true
  }
  if (pathname === '/' || pathname === '/index.html') {
    const params = new URLSearchParams(search)
    return params.get('app') === '1'
  }
  return false
}

function isInvitePath(pathname: string): boolean {
  const path = normalizePath(pathname)
  return path === '/invite' || path === '/invite.html' || path.startsWith('/invite/')
}

export default function Root() {
  const kind = legalKind(window.location.pathname)
  if (kind) return <LegalPage kind={kind} />
  if (isInvitePath(window.location.pathname)) return <InviteLanding />

  const openWorkspace = shouldOpenWorkspace(
    window.location.pathname,
    window.location.search,
  )

  if (!openWorkspace) {
    return <LandingPage />
  }

  return <UnifiedApp />
}
