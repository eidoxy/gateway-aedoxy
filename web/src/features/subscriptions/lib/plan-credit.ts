import type { UserSubscriptionRecord } from '../types'

export interface PlanCredit {
  /** At least one active plan has no quota cap (amount_total = 0). */
  unlimited: boolean
  /** Remaining quota summed over active, capped plans. */
  remaining: number
}

/** Sum the credit still spendable on the user's active plans. */
export function summarizePlanCredit(
  records: UserSubscriptionRecord[],
  nowSeconds: number = Math.floor(Date.now() / 1000)
): PlanCredit {
  const credit: PlanCredit = { unlimited: false, remaining: 0 }
  for (const record of records) {
    const sub = record.subscription
    if (sub.status !== 'active' || sub.end_time <= nowSeconds) continue
    if (sub.amount_total <= 0) {
      credit.unlimited = true
      continue
    }
    credit.remaining += Math.max(0, sub.amount_total - sub.amount_used)
  }
  return credit
}

/** Whether the user holds any active plan, capped or not. */
export function hasActivePlan(
  records: UserSubscriptionRecord[],
  nowSeconds: number = Math.floor(Date.now() / 1000)
): boolean {
  return records.some(
    (record) =>
      record.subscription.status === 'active' &&
      record.subscription.end_time > nowSeconds
  )
}
