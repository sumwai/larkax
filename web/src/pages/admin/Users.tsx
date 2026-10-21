import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api, ApiError } from '../../api'
import type {
  AdminUserDetail,
  AdminUserStatus,
  AdminUserSummary,
  Role,
  UserStatus,
} from '../../api/types'
import { useAuth } from '../../auth/AuthProvider'
import { useI18n } from '../../i18n'
import type { MessageKey } from '../../i18n/zh'
import { useSite } from '../../site/SiteProvider'
import { formatCount, formatDateTime } from '../../lib/format'
import {
  Alert,
  Button,
  Empty,
  Modal,
  MoneyText,
  PageHead,
  Pill,
  Skeleton,
  useToast,
} from '../../components/ui'
import { IconUser } from '../../components/icons'
import { OpsLoadError, SearchBox } from '../ops/common'

/** 搜索停手 300 毫秒再发请求：每敲一个字发一次，服务端看到的多半是半截词。 */
const SEARCH_DEBOUNCE_MS = 300
/** 一页五十行，与服务端的上限一致；再往下靠「加载更多」翻。 */
const PAGE_SIZE = 50

const ROLES: Role[] = ['user', 'merchant', 'admin']
const USER_STATUSES: UserStatus[] = ['active', 'suspended', 'banned']

const ROLE_LABEL: Record<Role, MessageKey> = {
  user: 'ops.users.role.user',
  merchant: 'ops.users.role.merchant',
  admin: 'ops.users.role.admin',
}
const ROLE_TONE = { user: 'neutral', merchant: 'warn', admin: 'accent' } as const

const STATUS_LABEL: Record<AdminUserStatus, MessageKey> = {
  active: 'ops.users.status.active',
  suspended: 'ops.users.status.suspended',
  banned: 'ops.users.status.banned',
  deletion_requested: 'ops.users.status.deletionRequested',
  deleted: 'ops.users.status.deleted',
}
const STATUS_TONE = {
  active: 'ok',
  suspended: 'warn',
  banned: 'bad',
  deletion_requested: 'neutral',
  deleted: 'neutral',
} as const

function RolePill({ role }: { role: Role }) {
  const { t } = useI18n()
  return <Pill tone={ROLE_TONE[role]}>{t(ROLE_LABEL[role])}</Pill>
}

function UserStatusPill({ status }: { status: AdminUserStatus }) {
  const { t } = useI18n()
  return (
    <Pill tone={STATUS_TONE[status]} dot>
      {t(STATUS_LABEL[status])}
    </Pill>
  )
}

/** 用户列表：服务端搜索、角色与状态筛选、游标翻页。 */
export function AdminUsers() {
  const { t } = useI18n()
  const { meta } = useSite()
  const client = api.admin

  const [typed, setTyped] = useState('')
  const [query, setQuery] = useState('')
  const [role, setRole] = useState<Role | ''>('')
  const [status, setStatus] = useState<UserStatus | ''>('')
  const [rows, setRows] = useState<AdminUserSummary[] | null>(null)
  const [nextCursor, setNextCursor] = useState<string | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const [reloadToken, setReloadToken] = useState(0)

  // 防抖：输入框里每变一次就重置计时，停手之后才把词放进真正查的那个 state。
  useEffect(() => {
    if (typed === query) return
    const timer = setTimeout(() => setQuery(typed), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(timer)
  }, [typed, query])

  const params = useMemo(
    () => ({
      query: query.trim() || undefined,
      role: role || undefined,
      status: status || undefined,
      limit: PAGE_SIZE,
    }),
    [query, role, status],
  )

  // 每按一组条件取第一页就换一次号。翻页带着出发时的号回来，号对不上就不写进
  // state：翻页在途时改了筛选，旧那一页不能追到新条件的列表里。
  const generation = useRef(0)

  useEffect(() => {
    let alive = true
    const token = (generation.current += 1)
    // 条件变了就从第一页重来：旧行留在屏幕上会冒充新条件的结果。
    setRows(null)
    setNextCursor(null)
    setError(null)
    client
      .listUsers(params)
      .then((page) => {
        if (!alive || token !== generation.current) return
        setRows(page.items)
        setNextCursor(page.next_cursor)
      })
      .catch((failure) => {
        if (alive && token === generation.current) setError(failure)
      })
    return () => {
      alive = false
    }
  }, [client, params, reloadToken])

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return
    const token = generation.current
    setLoadingMore(true)
    try {
      const page = await client.listUsers({ ...params, cursor: nextCursor })
      if (token !== generation.current) return
      // 追加，不替换：翻页时已经读到的行不该闪一下再回来。
      setRows((prev) => [...(prev ?? []), ...page.items])
      setNextCursor(page.next_cursor)
    } catch (failure) {
      if (token === generation.current) setError(failure)
    } finally {
      setLoadingMore(false)
    }
  }

  const filtering = query.trim() !== '' || role !== '' || status !== ''
  const clearFilters = () => {
    setTyped('')
    setQuery('')
    setRole('')
    setStatus('')
  }

  return (
    <>
      <PageHead title={t('ops.users.title')} subtitle={t('ops.users.subtitle')} />

      <div className="model-filters" style={{ marginBottom: 12 }}>
        <SearchBox value={typed} onChange={setTyped} />
        <select
          className="select"
          aria-label={t('ops.users.filterRole')}
          value={role}
          onChange={(event) => setRole(event.target.value as Role | '')}
        >
          <option value="">{t('ops.users.allRoles')}</option>
          {ROLES.map((value) => (
            <option key={value} value={value}>
              {t(ROLE_LABEL[value])}
            </option>
          ))}
        </select>
        <select
          className="select"
          aria-label={t('ops.users.filterStatus')}
          value={status}
          onChange={(event) => setStatus(event.target.value as UserStatus | '')}
        >
          <option value="">{t('ops.users.allStatuses')}</option>
          {USER_STATUSES.map((value) => (
            <option key={value} value={value}>
              {t(STATUS_LABEL[value])}
            </option>
          ))}
        </select>
      </div>

      {error && <OpsLoadError error={error} onRetry={() => setReloadToken((token) => token + 1)} />}

      {!rows && !error && (
        <div className="card panel">
          {[0, 1, 2, 3].map((row) => (
            <Skeleton key={row} height={18} />
          ))}
        </div>
      )}

      {rows && rows.length === 0 && filtering && (
        <div className="card">
          <Empty
            icon={<IconUser size={20} />}
            title={t('ops.users.noMatchTitle')}
            body={t('ops.users.noMatchBody')}
            action={
              <Button variant="secondary" onClick={clearFilters}>
                {t('ops.users.clearFilters')}
              </Button>
            }
          />
        </div>
      )}

      {rows && rows.length === 0 && !filtering && (
        <div className="card">
          <Empty
            icon={<IconUser size={20} />}
            title={t('ops.users.emptyTitle')}
            body={t('ops.users.emptyBody')}
          />
        </div>
      )}

      {rows && rows.length > 0 && (
        <section className="card table ops-table" aria-label={t('ops.users.title')}>
          <div className="thead users-head" style={{ display: 'grid' }}>
            <span>{t('ops.users.colUser')}</span>
            <span>{t('ops.users.colEmail')}</span>
            <span>{t('ops.users.colRole')}</span>
            <span>{t('ops.users.colStatus')}</span>
            <span>{t('ops.users.colCreated')}</span>
            <span>{t('ops.users.colLastSeen')}</span>
            <span className="amount">{t('ops.colActions')}</span>
          </div>
          {rows.map((row) => (
            <div className="trow users-row" key={row.id}>
              <div className="keyname">
                <b>{row.username}</b>
                <span className="muted-cell">{row.display_name || t('common.none')}</span>
              </div>
              {/* 列表里只有服务端打码过的邮箱：完整地址只在详情页出现。 */}
              <span className="mono">{row.email || t('common.none')}</span>
              <span>
                <RolePill role={row.role} />
              </span>
              <span>
                <UserStatusPill status={row.status} />
              </span>
              <span className="muted-cell num">{formatDateTime(row.created_at, meta.timezone)}</span>
              <span className="muted-cell num">
                {row.last_seen_at
                  ? formatDateTime(row.last_seen_at, meta.timezone)
                  : t('ops.users.neverSeen')}
              </span>
              <div className="actions">
                <Link className="btn ghost sm" to={`/app/admin/users/${row.id}`}>
                  {t('ops.view')}
                </Link>
              </div>
            </div>
          ))}
          {nextCursor && (
            <div className="table-foot" style={{ display: 'flex', justifyContent: 'center' }}>
              <Button variant="secondary" onClick={loadMore} loading={loadingMore}>
                {t('ops.users.loadMore')}
              </Button>
            </div>
          )}
        </section>
      )}
    </>
  )
}

type EditState = { kind: 'role'; value: Role } | { kind: 'status'; value: UserStatus }

/** 用户详情：账号资料、六个指标，以及改角色与改状态。 */
export function AdminUserDetailPage() {
  const { id: raw } = useParams<{ id: string }>()
  const { t, lang } = useI18n()
  const { meta } = useSite()
  const { user: me } = useAuth()
  const { show: toast } = useToast()

  const id = Number(raw)
  const validId = Number.isInteger(id) && id > 0

  const [detail, setDetail] = useState<AdminUserDetail | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [reloadToken, setReloadToken] = useState(0)
  const [edit, setEdit] = useState<EditState | null>(null)
  const [pending, setPending] = useState(false)
  const [failure, setFailure] = useState<{ status: number; message: string } | null>(null)

  useEffect(() => {
    if (!validId) return
    let alive = true
    setDetail(null)
    setError(null)
    api.admin
      .getUser(id)
      .then((next) => {
        if (alive) setDetail(next)
      })
      .catch((problem) => {
        if (alive) setError(problem)
      })
    return () => {
      alive = false
    }
  }, [validId, id, reloadToken])

  const submit = async () => {
    if (!edit || !detail) return
    setPending(true)
    setFailure(null)
    try {
      const next = await api.admin.updateUser(
        detail.id,
        edit.kind === 'role' ? { role: edit.value } : { status: edit.value },
      )
      // PATCH 的响应体与详情同形，直接用它刷新：不重发 GET，页面上的
      // 「改完之后」就不依赖两次请求之间世界没变。
      setDetail(next)
      setEdit(null)
      toast(t('ops.saved'))
    } catch (problem) {
      const known = problem instanceof ApiError
      // 4xx 的正文是服务端给的那句话（「an administrator cannot change their own
      // role」之类），原样展示：换成我们自己的话就把「为什么不行」丢掉了。
      setFailure({
        status: known ? problem.status : 0,
        message: known ? problem.message : t('ops.serverError'),
      })
      setEdit(null)
    } finally {
      setPending(false)
    }
  }

  if (!validId) {
    return (
      <>
        <PageHead title={t('ops.users.detailTitle')} />
        <Alert tone="bad" title={t('ops.users.badId')}>
          <Link to="/app/admin/users">{t('ops.users.backToList')}</Link>
        </Alert>
      </>
    )
  }

  const self = detail !== null && me !== null && me.id === detail.id
  const locked =
    detail !== null && (detail.status === 'deletion_requested' || detail.status === 'deleted')

  return (
    <>
      <PageHead
        title={t('ops.users.detailTitle')}
        subtitle={detail ? `#${detail.id} · ${detail.username}` : undefined}
        actions={
          detail && (
            <>
              <Link className="btn secondary" to="/app/admin/users">
                {t('ops.users.backToList')}
              </Link>
              <Button
                variant="secondary"
                disabled={self || locked || pending}
                onClick={() => {
                  setFailure(null)
                  setEdit({ kind: 'role', value: detail.role })
                }}
              >
                {t('ops.users.changeRole')}
              </Button>
              <Button
                variant="secondary"
                disabled={self || locked || pending}
                onClick={() => {
                  setFailure(null)
                  setEdit({
                    kind: 'status',
                    value: detail.status === 'active' ? 'suspended' : 'active',
                  })
                }}
              >
                {t('ops.users.changeStatus')}
              </Button>
            </>
          )
        }
      />

      {self && <p className="hint">{t('ops.users.selfHint')}</p>}
      {locked && detail && (
        <p className="hint">{t('ops.users.lockedHint', { status: t(STATUS_LABEL[detail.status]) })}</p>
      )}

      {failure && (
        <Alert tone="bad" title={failure.message}>
          {failure.status === 409 ? (
            <>
              {t('ops.users.conflictHint')}{' '}
              <Link to="/app/admin/users">{t('ops.users.backToList')}</Link>
            </>
          ) : null}
        </Alert>
      )}

      {!detail && !error && (
        <div className="card panel">
          {[0, 1, 2, 3].map((row) => (
            <Skeleton key={row} height={18} />
          ))}
        </div>
      )}

      {error && <OpsLoadError error={error} onRetry={() => setReloadToken((token) => token + 1)} />}

      {detail && (
        <>
          <section className="card panel" aria-label={t('ops.users.sectionProfile')}>
            <div className="sheet-rows">
              <div>
                <span>{t('ops.users.username')}</span>
                <b>{detail.username}</b>
              </div>
              <div>
                <span>{t('ops.users.displayName')}</span>
                <b>{detail.display_name || t('common.none')}</b>
              </div>
              {/* 完整邮箱只在详情页出现；列表里是服务端打码过的。 */}
              <div>
                <span>{t('ops.users.email')}</span>
                <b>{detail.email_full || t('ops.users.noEmail')}</b>
              </div>
              <div>
                <span>{t('ops.users.role')}</span>
                <span>
                  <RolePill role={detail.role} />
                </span>
              </div>
              <div>
                <span>{t('ops.users.status')}</span>
                <span>
                  <UserStatusPill status={detail.status} />
                </span>
              </div>
              <div>
                <span>{t('ops.users.createdAt')}</span>
                <b className="num">{formatDateTime(detail.created_at, meta.timezone)}</b>
              </div>
              <div>
                <span>{t('ops.users.lastSeen')}</span>
                <b className="num">
                  {detail.last_seen_at
                    ? formatDateTime(detail.last_seen_at, meta.timezone)
                    : t('ops.users.neverSeen')}
                </b>
              </div>
            </div>
          </section>

          <section className="kpis" aria-label={t('ops.users.sectionMetrics')}>
            <div className="card kpi">
              <div className="k">{t('ops.users.metricBalance')}</div>
              <div className="v">
                <MoneyText value={detail.balance} currency={meta.currency} variant="balance" />
              </div>
            </div>
            <div className="card kpi">
              <div className="k">{t('ops.users.metricKeys')}</div>
              <div className="v">{formatCount(detail.active_key_count, lang)}</div>
            </div>
            <div className="card kpi">
              <div className="k">{t('ops.users.metricRequests')}</div>
              <div className="v">{formatCount(detail.requests_7d, lang)}</div>
            </div>
            <div className="card kpi">
              <div className="k">{t('ops.users.metricSpend')}</div>
              <div className="v">
                <MoneyText value={detail.amount_7d} currency={meta.currency} variant="spend" />
              </div>
            </div>
            <div className="card kpi">
              <div className="k">{t('ops.users.metricChannels')}</div>
              <div className="v">{formatCount(detail.channel_count, lang)}</div>
            </div>
            <div className="card kpi">
              <div className="k">{t('ops.users.metricKeyUsed')}</div>
              <div className="v" style={{ fontSize: 16 }}>
                {detail.key_last_used_at
                  ? formatDateTime(detail.key_last_used_at, meta.timezone)
                  : t('ops.users.neverUsed')}
              </div>
            </div>
          </section>
        </>
      )}

      {edit && detail && (
        <Modal
          title={edit.kind === 'role' ? t('ops.users.changeRole') : t('ops.users.changeStatus')}
          description={
            edit.kind === 'role'
              ? `${t(ROLE_LABEL[detail.role])} → ${t(ROLE_LABEL[edit.value])}`
              : `${t(STATUS_LABEL[detail.status])} → ${t(STATUS_LABEL[edit.value])}`
          }
          onClose={() => {
            if (!pending) setEdit(null)
          }}
          footer={
            <>
              <Button variant="secondary" disabled={pending} onClick={() => setEdit(null)}>
                {t('common.cancel')}
              </Button>
              <Button
                onClick={submit}
                loading={pending}
                variant={edit.kind === 'status' && edit.value === 'banned' ? 'danger' : 'primary'}
              >
                {t('common.confirm')}
              </Button>
            </>
          }
        >
          <div className="field">
            <label htmlFor="user-edit-value">
              {edit.kind === 'role' ? t('ops.users.role') : t('ops.users.status')}
            </label>
            <select
              id="user-edit-value"
              className="select"
              value={edit.value}
              onChange={(event) => {
                const value = event.target.value
                setEdit(
                  edit.kind === 'role'
                    ? { kind: 'role', value: value as Role }
                    : { kind: 'status', value: value as UserStatus },
                )
              }}
            >
              {(edit.kind === 'role' ? ROLES : USER_STATUSES).map((value) => (
                <option key={value} value={value}>
                  {edit.kind === 'role'
                    ? t(ROLE_LABEL[value as Role])
                    : t(STATUS_LABEL[value as UserStatus])}
                </option>
              ))}
            </select>
            <span className="hint">{effectOf(edit, detail, t)}</span>
          </div>
        </Modal>
      )}
    </>
  )
}

/** 这一步的后果，写在这里而不是选了才看见——改角色、改状态都不是能点错了再说的操作。 */
function effectOf(
  edit: EditState,
  detail: AdminUserDetail,
  t: (key: MessageKey, vars?: Record<string, string | number>) => string,
): string {
  if (edit.kind === 'role') {
    if (edit.value === detail.role) return t('ops.users.roleUnchanged')
    if (detail.role === 'admin') return t('ops.users.roleFromAdmin')
    if (edit.value === 'admin') return t('ops.users.roleToAdmin')
    return t('ops.users.roleEffect')
  }
  if (edit.value === 'suspended') return t('ops.users.statusSuspended')
  if (edit.value === 'banned') return t('ops.users.statusBanned')
  return t('ops.users.statusActive')
}
