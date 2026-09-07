interface InviteTarget {
  code: string
  ref: string
  utm: Record<string, string>
}

/** Both /invite?code=X and /invite/X are accepted; ?ref=Y is a referral. */
export function parseInviteTarget(pathname: string, search: string): InviteTarget {
  const params = new URLSearchParams(search)
  const segment = pathname.replace(/\/+$/, '').split('/')[2] ?? ''
  const code = (params.get('code') ?? params.get('invite') ?? segment).trim()
  const ref = code ? '' : (params.get('ref') ?? '').trim()
  const utm: Record<string, string> = {}
  for (const key of ['utm_source', 'utm_medium', 'utm_campaign', 'utm_content']) {
    const value = params.get(key)?.trim()
    if (value) utm[key] = value
  }
  return { code, ref, utm }
}

/** The link worth sharing: the landing page without tracking parameters. */
export function canonicalInviteLink(origin: string, target: Pick<InviteTarget, 'code' | 'ref'>): string {
  const url = new URL('/invite', origin)
  if (target.code) url.searchParams.set('code', target.code)
  else if (target.ref) url.searchParams.set('ref', target.ref)
  return url.href
}

export function signupPath(target: Pick<InviteTarget, 'code' | 'ref'>): string {
  const params = new URLSearchParams()
  if (target.code) params.set('invite', target.code)
  else if (target.ref) params.set('ref', target.ref)
  const query = params.toString()
  return query ? `/pro?${query}` : '/pro'
}
