import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'

import { formatQuota } from '@/lib/format'
import { requireServerSuccess } from '@/lib/server-error-message'

import { getSelfSubscriptionFull } from '../api'
import { hasActivePlan, summarizePlanCredit } from '../lib/plan-credit'

export interface PlanCreditStat {
  label: string
  value: string
  description: string
}

/**
 * Plan credit of the signed-in user for the balance cards. Wallet quota stays
 * 0 for plan-only accounts, so the card must show the plan credit instead.
 * `stat` and `credit` are null while loading, on failure, or when the user has no active
 * plan; callers then keep their wallet balance display.
 */
export function usePlanCreditStat(walletQuota: number) {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: ['subscription', 'self', 'plan-credit'],
    queryFn: async () =>
      requireServerSuccess(await getSelfSubscriptionFull()).data,
    staleTime: 30 * 1000,
    meta: { errorToast: false },
  })

  const records = query.data?.subscriptions ?? []
  if (!hasActivePlan(records)) {
    return { isLoading: query.isLoading, stat: null, credit: null }
  }

  const credit = summarizePlanCredit(records)
  const stat: PlanCreditStat = {
    label: t('Plan credit'),
    value: credit.unlimited ? t('Unlimited') : formatQuota(credit.remaining),
    description: t('Wallet balance: {{amount}}', {
      amount: formatQuota(walletQuota),
    }),
  }
  return { isLoading: false, stat, credit }
}
