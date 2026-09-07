import type { Messages } from '../i18n'

/** Public view of an invitation; the server never includes channel data. */
export interface Offer {
  kind?: 'promotion' | 'referral'
  name?: string
  headline?: string
  description?: string
  grant_usd?: number
  grant_days?: number
  plan_code?: string
  plan_days?: number
  usage_discount_percent?: number
  discount_days?: number
  topup_bonus_percent?: number
  topup_bonus_days?: number
  milestone_session_usd?: number
  milestone_topup_usd?: number
  expires_at?: string
  max_registrations?: number
  remaining?: number
  referrer_name?: string
}

export function inviteApiBase(): string {
  return (import.meta.env.VITE_BACKEND_URL || 'http://localhost:8080').replace(/\/$/, '')
}

/** The reward lines an offer promises, in display order. */
export function offerRewardLines(offer: Offer, m: Messages): string[] {
  const lines: string[] = []
  if ((offer.grant_usd ?? 0) > 0) lines.push(m.auth.inviteCredit(offer.grant_usd!, offer.grant_days ?? 30))
  if (offer.plan_code) lines.push(m.auth.invitePlan(offer.plan_code, offer.plan_days ?? 30))
  if ((offer.usage_discount_percent ?? 0) > 0) lines.push(m.auth.inviteDiscount(offer.usage_discount_percent!, offer.discount_days ?? 30))
  if ((offer.topup_bonus_percent ?? 0) > 0) lines.push(m.auth.inviteTopupBonus(offer.topup_bonus_percent!, offer.topup_bonus_days ?? 30))
  if ((offer.milestone_topup_usd ?? 0) > 0) lines.push(m.auth.inviteMilestoneTopup(offer.milestone_topup_usd!))
  if ((offer.milestone_session_usd ?? 0) > 0) lines.push(m.auth.inviteMilestoneSession(offer.milestone_session_usd!))
  return lines
}
