import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { PaymentOrder } from '@/types/payment'
import OrderTable from '../OrderTable.vue'
import AdminOrderTable from '@/components/admin/payment/AdminOrderTable.vue'
import AdminOrderDetail from '@/components/admin/payment/AdminOrderDetail.vue'
import AdminRefundDialog from '@/components/admin/payment/AdminRefundDialog.vue'
import PaymentQRDialog from '../PaymentQRDialog.vue'
import StripePaymentInline from '../StripePaymentInline.vue'
import AdminOrdersView from '@/views/admin/orders/AdminOrdersView.vue'
import StripePaymentView from '@/views/user/StripePaymentView.vue'
import StripePopupView from '@/views/user/StripePopupView.vue'

const pollOrderStatus = vi.hoisted(() => vi.fn())
const cancelOrder = vi.hoisted(() => vi.fn())
const verifyOrder = vi.hoisted(() => vi.fn())
const getOrder = vi.hoisted(() => vi.fn())
const showError = vi.hoisted(() => vi.fn())
const showSuccess = vi.hoisted(() => vi.fn())
const toCanvas = vi.hoisted(() => vi.fn())
const confirmPayment = vi.hoisted(() => vi.fn())
const adminGetOrders = vi.hoisted(() => vi.fn())
const adminGetOrder = vi.hoisted(() => vi.fn())
const adminCancelOrder = vi.hoisted(() => vi.fn())
const adminRetryRecharge = vi.hoisted(() => vi.fn())
const adminRefundOrder = vi.hoisted(() => vi.fn())
const routeState = vi.hoisted(() => ({
  query: {} as Record<string, unknown>,
}))
const paymentStore = vi.hoisted(() => ({
  config: { stripe_publishable_key: 'pk_test' } as { stripe_publishable_key?: string },
  fetchConfig: vi.fn(),
  pollOrderStatus,
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
      locale: 'en-US',
    }),
  }
})

vi.mock('@/stores/payment', () => ({
  usePaymentStore: () => paymentStore,
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    showError,
  }),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
  }),
}))

vi.mock('@/api/payment', () => ({
  paymentAPI: {
    cancelOrder,
    verifyOrder,
    getOrder,
  },
}))

vi.mock('@/api/admin/payment', () => ({
  adminPaymentAPI: {
    getOrders: adminGetOrders,
    getOrder: adminGetOrder,
    cancelOrder: adminCancelOrder,
    retryRecharge: adminRetryRecharge,
    refundOrder: adminRefundOrder,
  },
  default: {
    getOrders: adminGetOrders,
    getOrder: adminGetOrder,
    cancelOrder: adminCancelOrder,
    retryRecharge: adminRetryRecharge,
    refundOrder: adminRefundOrder,
  },
}))

vi.mock('qrcode', () => ({
  default: {
    toCanvas,
  },
}))

vi.mock('vue-router', () => ({
  useRoute: () => routeState,
  useRouter: () => ({
    push: vi.fn(),
    resolve: (to: { path: string; query?: Record<string, string | undefined> }) => ({
      href: `${to.path}?${new URLSearchParams(
        Object.entries(to.query || {}).flatMap(([key, value]) => value ? [[key, value]] : []),
      ).toString()}`,
    }),
  }),
}))

vi.mock('@stripe/stripe-js', () => ({
  loadStripe: vi.fn(async () => ({
    elements: () => ({
      create: () => ({
        mount: vi.fn(),
        on: (event: string, callback: (payload?: { value: { type: string } }) => void) => {
          if (event === 'change') {
            callback({ value: { type: 'card' } })
            return
          }
          callback()
        },
      }),
    }),
    confirmAlipayPayment: vi.fn(),
    confirmWechatPayPayment: vi.fn(),
    confirmPayment,
  })),
}))

const DataTableStub = {
  props: ['data'],
  template: `
    <div>
      <div v-for="row in data" :key="row.id">
        <slot name="cell-pay_amount" :row="row" :value="row.pay_amount" />
      </div>
    </div>
  `,
}

const AdminOrderTableStub = {
  props: ['orders', 'loading', 'showUser'],
  template: `
    <div>
      <div v-for="row in orders" :key="row.id">
        <slot name="actions" :row="row" />
      </div>
    </div>
  `,
}

const AdminRefundDialogStub = {
  props: ['show', 'order', 'submitting', 'requireForce', 'warning'],
  emits: ['confirm', 'cancel'],
  template: '<div v-if="show" data-testid="refund-dialog" />',
}

function mountAdminOrdersView() {
  return mount(AdminOrdersView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        OrderTable: AdminOrderTableStub,
        Pagination: true,
        BaseDialog: {
          props: ['show'],
          template: '<div v-if="show"><slot /><slot name="footer" /></div>',
        },
        Select: true,
        Icon: true,
        OrderStatusBadge: true,
        AdminRefundDialog: AdminRefundDialogStub,
      },
    },
  })
}

function orderFactory(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  return {
    id: 42,
    user_id: 9,
    amount: 10,
    pay_amount: 1200,
    currency: 'JPY',
    fee_rate: 0,
    payment_type: 'alipay',
    out_trade_no: 'sub2_202607030001',
    status: 'COMPLETED',
    order_type: 'balance',
    created_at: '2026-07-03T12:00:00Z',
    expires_at: '2099-01-01T12:30:00Z',
    refund_amount: 0,
    ...overrides,
  }
}

function orderFactoryOmittingCurrency(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  const order = orderFactory(overrides)
  delete order.currency
  return order
}

describe('order currency display', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    pollOrderStatus.mockReset()
    cancelOrder.mockReset()
    verifyOrder.mockReset()
    getOrder.mockReset()
    showError.mockReset()
    showSuccess.mockReset()
    adminGetOrders.mockReset().mockResolvedValue({ data: { items: [], total: 0 } })
    adminGetOrder.mockReset()
    adminCancelOrder.mockReset()
    adminRetryRecharge.mockReset()
    adminRefundOrder.mockReset()
    toCanvas.mockReset().mockResolvedValue(undefined)
    confirmPayment.mockReset().mockResolvedValue({})
    paymentStore.config = { stripe_publishable_key: 'pk_test' }
    paymentStore.fetchConfig.mockReset().mockResolvedValue(undefined)
    routeState.query = {}
    window.localStorage.clear()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('displays order table pay_amount with order currency and keeps balance credited amount in USD', () => {
    const wrapper = mount(OrderTable, {
      props: {
        orders: [orderFactory()],
        loading: false,
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          OrderStatusBadge: true,
        },
      },
    })

    expect(wrapper.text()).toContain('¥1,200')
    expect(wrapper.text()).toContain('payment.orders.creditedAmount: $10.00')
    expect(wrapper.text()).not.toContain('¥10')
    expect(wrapper.text()).not.toContain('¥1,200.00')
  })

  it('displays admin order table pay_amount with order currency and keeps balance credited amount in USD', () => {
    const wrapper = mount(OrderTable, {
      props: {
        showUser: true,
        orders: [
          orderFactory({ id: 1, order_type: 'balance', amount: 10, pay_amount: 1200, currency: 'JPY' }),
          orderFactory({ id: 2, order_type: 'subscription', amount: 100, pay_amount: 101, currency: 'HKD' }),
          orderFactory({ id: 3, order_type: 'subscription', amount: 200, pay_amount: 202, currency: 'USD' }),
          orderFactory({ id: 4, order_type: 'subscription', amount: 300, pay_amount: 303, currency: 'CNY' }),
          orderFactory({ id: 5, order_type: 'subscription', amount: 12.3, pay_amount: 12.3, currency: 'bad-currency' }),
        ],
        loading: false,
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          OrderStatusBadge: true,
        },
      },
    })

    expect(wrapper.text()).toContain('¥1,200')
    expect(wrapper.text()).not.toContain('¥1,200.00')
    expect(wrapper.text()).toContain('payment.orders.creditedAmount: $10.00')
    expect(wrapper.text()).not.toContain('payment.orders.creditedAmount: ¥10')
    expect(wrapper.text()).toContain('$101.00')
    expect(wrapper.text()).toContain('$100.00')
    expect(wrapper.text()).toContain('$202.00')
    expect(wrapper.text()).toContain('$200.00')
    expect(wrapper.text()).toContain('¥303.00')
    expect(wrapper.text()).toContain('¥300.00')
    expect(wrapper.text()).toContain('¥12.30')
  })

  it('falls back to CNY in admin order table when order currency is omitted, undefined, or null', () => {
    const wrapper = mount(OrderTable, {
      props: {
        showUser: true,
        orders: [
          orderFactoryOmittingCurrency({ id: 1, order_type: 'subscription', amount: 12.3, pay_amount: 45.6 }),
          orderFactory({ id: 2, order_type: 'subscription', amount: 20, pay_amount: 21, currency: undefined }),
          orderFactory({ id: 3, order_type: 'subscription', amount: 30, pay_amount: 31, currency: null as unknown as string }),
        ],
        loading: false,
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          OrderStatusBadge: true,
        },
      },
    })

    expect(wrapper.text()).toContain('¥45.60')
    expect(wrapper.text()).toContain('¥12.30')
    expect(wrapper.text()).toContain('¥21.00')
    expect(wrapper.text()).toContain('¥20.00')
    expect(wrapper.text()).toContain('¥31.00')
    expect(wrapper.text()).toContain('¥30.00')
  })

  it('keeps subscription order amounts in the payment currency for HKD, USD, and JPY', () => {
    const wrapper = mount(OrderTable, {
      props: {
        orders: [
          orderFactory({ id: 1, order_type: 'subscription', amount: 100, pay_amount: 101, currency: 'HKD' }),
          orderFactory({ id: 2, order_type: 'subscription', amount: 200, pay_amount: 202, currency: 'USD' }),
          orderFactory({ id: 3, order_type: 'subscription', amount: 300, pay_amount: 303, currency: 'JPY' }),
        ],
        loading: false,
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          OrderStatusBadge: true,
        },
      },
    })

    expect(wrapper.text()).toContain('$101.00')
    expect(wrapper.text()).toContain('$100.00')
    expect(wrapper.text()).toContain('$202.00')
    expect(wrapper.text()).toContain('$200.00')
    expect(wrapper.text()).toContain('¥303')
    expect(wrapper.text()).toContain('¥300')
    expect(wrapper.text()).not.toContain('$303.00')
  })

  it('uses order currency in QR dialog success and does not hard-code yen', async () => {
    pollOrderStatus.mockResolvedValue(orderFactory({
      amount: 100,
      pay_amount: 108,
      currency: 'USD',
      order_type: 'subscription',
    }))

    const wrapper = mount(PaymentQRDialog, {
      props: {
        show: false,
        orderId: 42,
        qrCode: '',
        expiresAt: '2099-01-01T12:30:00Z',
        paymentType: 'alipay',
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>',
          },
          Icon: true,
        },
      },
    })

    await wrapper.setProps({ show: true })
    await flushPromises()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(wrapper.text()).toContain('$100.00')
    expect(wrapper.text()).toContain('$108.00')
    expect(wrapper.text()).not.toContain('¥108')
  })

  it('uses order currency in reachable Stripe route amount display and does not hard-code yen', async () => {
    routeState.query = {
      order_id: '42',
      client_secret: 'pi_secret',
    }
    getOrder.mockResolvedValue({
      data: orderFactory({
        amount: 50,
        pay_amount: 5000,
        currency: 'USD',
        payment_type: 'stripe',
      }),
    })

    const wrapper = mount(StripePaymentView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          Icon: true,
        },
      },
    })

    await flushPromises()
    await flushPromises()

    expect(getOrder).toHaveBeenCalledWith(42)
    expect(wrapper.text()).toContain('$5,000.00')
    expect(wrapper.text()).not.toContain('¥5,000')
  })

  it('renders admin detail modal pay_amount with provider currency and balance amount separately', async () => {
    const rowOrder = orderFactory({
      amount: 10,
      pay_amount: 1200,
      currency: 'JPY',
      order_type: 'balance',
    })
    adminGetOrders.mockResolvedValue({
      data: {
        items: [rowOrder],
        total: 1,
      },
    })
    adminGetOrder.mockResolvedValue({
      data: {
        order: rowOrder,
        auditLogs: [],
      },
    })

    const wrapper = mount(AdminOrdersView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          OrderTable: AdminOrderTableStub,
          Pagination: true,
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>',
          },
          Select: true,
          Icon: true,
          OrderStatusBadge: true,
          AdminRefundDialog: true,
        },
      },
    })

    await flushPromises()
    const detailButton = wrapper.findAll('button').find(button => button.text().includes('common.view'))
    expect(detailButton).toBeTruthy()
    await detailButton!.trigger('click')
    await flushPromises()

    expect(adminGetOrder).toHaveBeenCalledWith(42)
    expect(wrapper.text()).toContain('$10.00')
    expect(wrapper.text()).toContain('¥1,200')
    expect(wrapper.text()).not.toContain('¥1,200.00')
    expect(wrapper.text()).not.toContain('¥10.00')
  })

  it('falls back to CNY in admin detail modal when order currency is omitted', async () => {
    const rowOrder = orderFactoryOmittingCurrency({
      amount: 100,
      pay_amount: 101,
      order_type: 'subscription',
    })
    adminGetOrders.mockResolvedValue({
      data: {
        items: [rowOrder],
        total: 1,
      },
    })
    adminGetOrder.mockResolvedValue({
      data: {
        order: rowOrder,
        auditLogs: [],
      },
    })

    const wrapper = mount(AdminOrdersView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          OrderTable: AdminOrderTableStub,
          Pagination: true,
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>',
          },
          Select: true,
          Icon: true,
          OrderStatusBadge: true,
          AdminRefundDialog: true,
        },
      },
    })

    await flushPromises()
    const detailButton = wrapper.findAll('button').find(button => button.text().includes('common.view'))
    expect(detailButton).toBeTruthy()
    await detailButton!.trigger('click')
    await flushPromises()

    expect(adminGetOrder).toHaveBeenCalledWith(42)
    expect(wrapper.text()).toContain('¥100.00')
    expect(wrapper.text()).toContain('¥101.00')
  })

  it('keeps the refund dialog open and requests force after a partial-success response', async () => {
    const order = orderFactory({ status: 'COMPLETED' })
    adminGetOrders.mockResolvedValue({ data: { items: [order], total: 1 } })
    adminRefundOrder.mockResolvedValue({
      data: {
        success: false,
        warning: 'user balance is insufficient for deduction, use force',
        require_force: true,
      },
    })
    const wrapper = mountAdminOrdersView()
    await flushPromises()

    const refundButton = wrapper.findAll('button').find(button => button.text().includes('payment.admin.refund'))
    expect(refundButton).toBeTruthy()
    await refundButton!.trigger('click')

    const dialog = wrapper.getComponent(AdminRefundDialogStub)
    await dialog.vm.$emit('confirm', { amount: 10, reason: '', deduct_balance: true, force: false })
    await flushPromises()

    expect(adminRefundOrder).toHaveBeenCalledWith(42, {
      amount: 10,
      reason: '',
      deduct_balance: true,
      force: false,
    })
    expect(dialog.props('show')).toBe(true)
    expect(dialog.props('requireForce')).toBe(true)
    expect(dialog.props('warning')).toBe('user balance is insufficient for deduction, use force')
    expect(showSuccess).not.toHaveBeenCalled()
    expect(showError).not.toHaveBeenCalled()
  })

  it('uses order currency in legacy admin order table amount cells', () => {
    const wrapper = mount(AdminOrderTable, {
      props: {
        orders: [orderFactory({ amount: 10, pay_amount: 1200, currency: 'JPY', order_type: 'balance' })],
        loading: false,
        page: 1,
        pageSize: 20,
        total: 1,
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          Pagination: true,
          Select: true,
          Icon: true,
        },
      },
    })

    expect(wrapper.text()).toContain('¥1,200')
    expect(wrapper.text()).toContain('payment.orders.creditedAmount: $10.00')
    expect(wrapper.text()).not.toContain('¥10.00')
    expect(wrapper.text()).not.toContain('¥1,200.00')
  })

  it('uses order currency in legacy admin order detail gateway amounts', () => {
    const wrapper = mount(AdminOrderDetail, {
      props: {
        show: true,
        order: orderFactory({
          amount: 10,
          pay_amount: 1200,
          currency: 'JPY',
          order_type: 'balance',
          refund_amount: 2,
        }),
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>',
          },
        },
      },
    })

    expect(wrapper.text()).toContain('¥1,200')
    expect(wrapper.text()).toContain('$10.00')
    expect(wrapper.text()).toContain('$2.00')
    expect(wrapper.text()).not.toContain('¥10.00')
    expect(wrapper.text()).not.toContain('¥2.00')
  })

  it('distinguishes balance product amounts from gateway pay amount in admin refund dialog', () => {
    const wrapper = mount(AdminRefundDialog, {
      props: {
        show: true,
        order: orderFactory({
          amount: 10,
          pay_amount: 1200,
          currency: 'JPY',
          order_type: 'balance',
          status: 'PARTIALLY_REFUNDED',
          refund_amount: 2,
        }),
        userBalance: 8,
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>',
          },
        },
      },
    })

    expect(wrapper.text()).toContain('payment.orders.creditedAmount')
    expect(wrapper.text()).toContain('$10.00')
    expect(wrapper.text()).toContain('¥1,200')
    expect(wrapper.text()).toContain('payment.admin.alreadyRefunded')
    expect(wrapper.text()).toContain('$2.00')
    expect(wrapper.text()).toContain('payment.admin.userBalance')
    expect(wrapper.text()).toContain('$8.00')
    expect(wrapper.text()).toContain('payment.admin.maxRefundable: $8.00')
    expect(wrapper.text()).not.toContain('¥10.00')
    expect(wrapper.text()).not.toContain('¥2.00')
  })

  it('does not hard-code yen for admin refund dialog gateway payment currency', () => {
    const wrapper = mount(AdminRefundDialog, {
      props: {
        show: true,
        order: orderFactory({
          amount: 50,
          pay_amount: 50,
          currency: 'USD',
          order_type: 'subscription',
          status: 'COMPLETED',
          refund_amount: 0,
        }),
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>',
          },
        },
      },
    })

    expect(wrapper.text()).toContain('$50.00')
    expect(wrapper.text()).not.toContain('¥50.00')
  })

  it('falls back to CNY in admin refund dialog gateway amount when order currency is null', () => {
    const wrapper = mount(AdminRefundDialog, {
      props: {
        show: true,
        order: orderFactory({
          amount: 50,
          pay_amount: 51,
          currency: null as unknown as string,
          order_type: 'subscription',
          status: 'COMPLETED',
          refund_amount: 0,
        }),
      },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show"><slot /><slot name="footer" /></div>',
          },
        },
      },
    })

    expect(wrapper.text()).toContain('¥50.00')
    expect(wrapper.text()).toContain('¥51.00')
  })

  it('uses currency prop in legacy Stripe inline amount display', async () => {
    const wrapper = mount(StripePaymentInline, {
      props: {
        orderId: 42,
        amount: 50,
        clientSecret: 'pi_secret',
        orderType: 'subscription',
        publishableKey: 'pk_test',
        payAmount: 51,
        currency: 'USD',
      },
      global: {
        stubs: {
          Icon: true,
        },
      },
    })

    await flushPromises()
    await flushPromises()

    expect(wrapper.text()).toContain('$51.00')
    expect(wrapper.text()).not.toContain('¥51.00')
  })

  it('uses currency query in legacy Stripe popup amount display', () => {
    routeState.query = {
      order_id: '42',
      method: 'alipay',
      amount: '51',
      currency: 'USD',
    }

    const wrapper = mount(StripePopupView)

    expect(wrapper.text()).toContain('$51.00')
    expect(wrapper.text()).not.toContain('¥51')
  })
})
