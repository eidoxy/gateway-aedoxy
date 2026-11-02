import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import { createElement, type ReactNode } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { usePlanCreditStat } from '../hooks/use-plan-credit'
import { summarizePlanCredit } from '../lib/plan-credit'
import type { UserSubscriptionRecord } from '../types'

const NOW = 1_800_000_000

function record(
  overrides: Partial<UserSubscriptionRecord['subscription']>
): UserSubscriptionRecord {
  return {
    subscription: {
      id: 1,
      user_id: 4,
      plan_id: 1,
      status: 'active',
      start_time: NOW - 100,
      end_time: NOW + 1000,
      amount_total: 20_000_000,
      amount_used: 0,
      ...overrides,
    },
  }
}

describe('summarizePlanCredit', () => {
  test('sums the remaining quota of every active plan', () => {
    const credit = summarizePlanCredit(
      [
        record({ amount_total: 20_000_000, amount_used: 5_000_000 }),
        record({ id: 2, amount_total: 1_000_000, amount_used: 0 }),
      ],
      NOW
    )
    expect(credit).toEqual({ unlimited: false, remaining: 16_000_000 })
  })

  test('ignores expired and non-active plans', () => {
    const credit = summarizePlanCredit(
      [
        record({ end_time: NOW - 1 }),
        record({ id: 2, status: 'expired' }),
        record({ id: 3, amount_total: 100, amount_used: 40 }),
      ],
      NOW
    )
    expect(credit).toEqual({ unlimited: false, remaining: 60 })
  })

  test('never reports negative credit for an overdrawn plan', () => {
    const credit = summarizePlanCredit(
      [record({ amount_total: 100, amount_used: 150 })],
      NOW
    )
    expect(credit.remaining).toBe(0)
  })

  test('flags a plan without a quota cap as unlimited', () => {
    const credit = summarizePlanCredit([record({ amount_total: 0 })], NOW)
    expect(credit.unlimited).toBe(true)
  })

  test('returns zero credit when there are no plans', () => {
    expect(summarizePlanCredit([], NOW)).toEqual({
      unlimited: false,
      remaining: 0,
    })
  })
})

describe('usePlanCreditStat', () => {
  afterEach(() => vi.restoreAllMocks())

  function renderStat(subscriptions: UserSubscriptionRecord[]) {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          billing_preference: 'subscription_first',
          subscriptions,
          all_subscriptions: subscriptions,
        },
      },
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const wrapper = ({ children }: { children: ReactNode }) =>
      createElement(QueryClientProvider, { client }, children)
    return renderHook(() => usePlanCreditStat(0), { wrapper })
  }

  test('shows plan credit next to a zero wallet balance once loaded', async () => {
    const { result } = renderStat([
      record({
        end_time: Math.floor(Date.now() / 1000) + 3600,
        amount_total: 20_000_000,
        amount_used: 0,
      }),
    ])
    expect(result.current.isLoading).toBe(true)
    expect(result.current.stat).toBeNull()

    await waitFor(() => expect(result.current.stat).not.toBeNull())
    expect(result.current.isLoading).toBe(false)
    expect(result.current.credit).toEqual({
      unlimited: false,
      remaining: 20_000_000,
    })
    expect(result.current.stat?.label).toBe('Plan credit')
  })

  test('keeps the wallet display when the user has no active plan', async () => {
    const { result } = renderStat([])
    await waitFor(() => expect(result.current.isLoading).toBe(false))
    expect(result.current.stat).toBeNull()
    expect(result.current.credit).toBeNull()
  })
})
