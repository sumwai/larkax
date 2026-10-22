import { useEffect, useId, useState } from 'react'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'

/**
 * 计价表达式的输入。
 *
 * 表单模式只覆盖「一张固定的单价表」：五个键各一行，数值与单位分开填。
 * 换算与校验全部走十进制字符串，不经过浮点——每 token 的单价低到 1e-7，
 * 这个量级上浮点乘一遍就会带出 `0.09999999999999999` 这类尾巴，而服务端按
 * 8 位小数存储。
 * 带条件的表达式（`len <= 32000 ? {...} : {...}`）只能走表达式模式。
 */

const SLOTS = ['prompt', 'completion', 'cache_read', 'cache_write', 'reasoning'] as const
type Slot = (typeof SLOTS)[number]

const LABELS: Record<Slot, MessageKey> = {
  prompt: 'ops.expr.prompt',
  completion: 'ops.expr.completion',
  cache_read: 'ops.expr.cacheRead',
  cache_write: 'ops.expr.cacheWrite',
  reasoning: 'ops.expr.reasoning',
}

type Unit = 'million' | 'thousand' | 'token'

/** 每个单位换算到「每 token」要左移的小数位数。 */
const UNIT_SHIFT: Record<Unit, number> = { million: 6, thousand: 3, token: 0 }

type Rows = Record<Slot, string>
type Units = Record<Slot, Unit>
type Mode = 'form' | 'expr'
type Translate = ReturnType<typeof useI18n>['t']

const EMPTY_ROWS: Rows = {
  prompt: '',
  completion: '',
  cache_read: '',
  cache_write: '',
  reasoning: '',
}

const DEFAULT_UNITS: Units = {
  prompt: 'million',
  completion: 'million',
  cache_read: 'million',
  cache_write: 'million',
  reasoning: 'million',
}

/** 服务端按 8 位小数存单价（`internal/money.Scale`），超出就是会被舍掉的部分。 */
const MAX_DECIMALS = 8

/** `internal/money.Parse` 能表示的最大幅值，负数侧多一格，这里只处理非负。 */
const MAX_UNITS = 9223372036854775807n

const DECIMAL = /^\d+(?:\.\d+)?$/

interface EditorState {
  mode: Mode
  rows: Rows
  units: Units
  text: string
  switchError: string | null
}

/**
 * 十进制字符串除以 10^places。
 *
 * 只做小数点搬移：`0.1` 除 10^6 得 `0.0000001`。不经过 Number，所以不会有
 * `1.0000000000000001e-7` 这类尾巴——`1e-7` 这一档才会露出浮点，`3.5` 那一档
 * 恰好是干净的，拿它举证证明不了什么。
 */
function divideByPower(value: string, places: number): string {
  const [whole, frac = ''] = value.split('.')
  const digits = (whole + frac).replace(/^0+/, '') || '0'
  if (digits === '0') return '0'
  const decimals = frac.length + places
  if (decimals === 0) return digits
  if (digits.length <= decimals) {
    return `0.${'0'.repeat(decimals - digits.length)}${digits}`
  }
  const cut = digits.length - decimals
  return trimDecimal(digits.slice(0, cut), digits.slice(cut))
}

/** 十进制字符串乘以 10^places，同样只搬小数点。 */
function multiplyByPower(value: string, places: number): string {
  const [whole, frac = ''] = value.split('.')
  const digits = whole + frac + '0'.repeat(Math.max(0, places - frac.length))
  const decimals = Math.max(0, frac.length - places)
  if (decimals === 0) return trimDecimal(digits, '')
  const cut = digits.length - decimals
  return trimDecimal(digits.slice(0, cut), digits.slice(cut))
}

function trimDecimal(whole: string, frac: string): string {
  const head = whole.replace(/^0+(?=\d)/, '') || '0'
  const tail = frac.replace(/0+$/, '')
  return tail ? `${head}.${tail}` : head
}

/** 输入的单位价换算成每 token 的单价；格式不对返回 null。 */
function toToken(value: string, unit: Unit): string | null {
  if (!DECIMAL.test(value)) return null
  return divideByPower(value, UNIT_SHIFT[unit])
}

/**
 * 一行的校验。报错要说清是哪一行——五行长得一样，不点名等于没说。
 *
 * 位数上限按服务端存单价的精度（8 位小数）来：再多的位会被四舍五入，
 * 界面上显示的值与实际生效的值就对不上了。
 */
function rowProblem(raw: string, unit: Unit, row: number, t: Translate): string | null {
  if (!DECIMAL.test(raw)) return t('ops.expr.invalidNumber', { row })
  const token = divideByPower(raw, UNIT_SHIFT[unit])
  const [whole, frac = ''] = token.split('.')
  // 尾零不占位数：`0.123456780` 的值只有 8 位小数，服务端原样存得下。
  if (frac.replace(/0+$/, '').length > MAX_DECIMALS) return t('ops.expr.precision', { row })
  const units = BigInt(whole || '0') * 100000000n + BigInt((frac + '00000000').slice(0, MAX_DECIMALS))
  if (units > MAX_UNITS) return t('ops.expr.tooLarge', { row })
  return null
}

function firstRowProblem(rows: Rows, units: Units, t: Translate): string | null {
  for (let index = 0; index < SLOTS.length; index++) {
    const slot = SLOTS[index]
    const raw = rows[slot].trim()
    if (!raw) continue
    const problem = rowProblem(raw, units[slot], index + 1, t)
    if (problem) return problem
  }
  return null
}

function formProblem(rows: Rows, units: Units, t: Translate): string | null {
  const problem = firstRowProblem(rows, units, t)
  if (problem) return problem
  const filled = SLOTS.some((slot) => rows[slot].trim() !== '')
  return filled ? null : t('ops.expr.atLeastOne')
}

/** 把五行拼成一条表达式；键的顺序固定，同样的输入每次得到同样的串。 */
function serialize(rows: Rows, units: Units): string {
  const parts: string[] = []
  for (const slot of SLOTS) {
    const raw = rows[slot].trim()
    if (!raw) continue
    const token = toToken(raw, units[slot])
    if (token === null) continue
    parts.push(`"${slot}": ${token}`)
  }
  return parts.length > 0 ? `{${parts.join(', ')}}` : ''
}

/**
 * 只在能按「五项固定单价」严格解析时返回行数据。
 *
 * 数字按字面量读，不经过 JSON.parse：`0.0000015` 一旦变成浮点，再写回去
 * 就未必是同一个串。带条件、带其它键、值不是裸十进制字面量，一律返回 null，
 * 由调用方留在表达式模式。
 */
function parseSimpleExpr(source: string): { rows: Rows; units: Units } | null {
  const text = source.trim()
  let index = 0
  const skip = () => {
    while (index < text.length && /\s/.test(text[index])) index++
  }

  const rows: Rows = { ...EMPTY_ROWS }
  const units: Units = { ...DEFAULT_UNITS }

  skip()
  if (text[index] !== '{') return null
  index++
  skip()
  if (text[index] === '}') {
    index++
    skip()
    return index === text.length ? { rows, units } : null
  }

  const seen = new Set<string>()
  for (;;) {
    skip()
    const key = /^(?:"([A-Za-z_][A-Za-z0-9_]*)"|([A-Za-z_][A-Za-z0-9_]*))/.exec(text.slice(index))
    if (!key) return null
    const name = key[1] ?? key[2]
    if (!(SLOTS as readonly string[]).includes(name) || seen.has(name)) return null
    seen.add(name)
    index += key[0].length

    skip()
    if (text[index] !== ':') return null
    index++
    skip()

    const literal = /^\d+(?:\.\d+)?/.exec(text.slice(index))
    if (!literal) return null
    index += literal[0].length

    // 展示一律按「每 100 万 token」：换算是右移六位，仍用字符串做。
    rows[name as Slot] = multiplyByPower(literal[0], UNIT_SHIFT.million)
    units[name as Slot] = 'million'

    skip()
    if (text[index] === ',') {
      index++
      continue
    }
    if (text[index] === '}') {
      index++
      break
    }
    return null
  }

  skip()
  return index === text.length ? { rows, units } : null
}

/**
 * 把一条「五项固定单价」的表达式换算成每 100 万 token 的口径，给展示用。
 *
 * 带条件、带别的键、值不是裸十进制字面量的表达式返回 `null`，调用方原样显示——
 * 那些不是一张单价表，硬乘会把 `len <= 32000 ? … : …` 变成一句无意义的话。
 * 换算走 `multiplyByPower`（只搬小数点），所以 `0.0000001` 是 `0.1`；
 * 同一个值用浮点乘会得到 `0.09999999999999999`。
 */
export function exprPerMillion(source: string): string | null {
  const parsed = parseSimpleExpr(source)
  if (!parsed) return null
  const parts: string[] = []
  for (const slot of SLOTS) {
    if (!parsed.rows[slot]) continue
    parts.push(`"${slot}": ${parsed.rows[slot]}`)
  }
  return `{${parts.join(', ')}}`
}

function initialState(value: string): EditorState {
  if (!value.trim()) {
    return { mode: 'form', rows: EMPTY_ROWS, units: DEFAULT_UNITS, text: '', switchError: null }
  }
  const parsed = parseSimpleExpr(value)
  if (parsed) {
    return { mode: 'form', rows: parsed.rows, units: parsed.units, text: value, switchError: null }
  }
  return { mode: 'expr', rows: EMPTY_ROWS, units: DEFAULT_UNITS, text: value, switchError: null }
}

/**
 * 计价规则里的单价输入。
 *
 * `value` 只在挂载时读一次：表单打开时它来自已有规则，之后由本组件通过
 * `onChange` 往外写。`onErrorChange` 报的是表单模式下的第一条错误（含「一项
 * 都没填」）；表达式模式不在这里校验，交给服务端编译。
 */
export function ExprEditor({
  value,
  onChange,
  onErrorChange,
}: {
  value: string
  onChange: (expr: string) => void
  onErrorChange: (error: string | null) => void
}) {
  const { t } = useI18n()
  const id = useId()
  const [state, setState] = useState(() => initialState(value))

  const problem = state.mode === 'form' ? formProblem(state.rows, state.units, t) : null
  useEffect(() => {
    onErrorChange(problem)
  }, [problem, onErrorChange])

  const setRow = (slot: Slot, raw: string, unit: Unit) => {
    const rows = { ...state.rows, [slot]: raw }
    const units = { ...state.units, [slot]: unit }
    setState({ ...state, rows, units, switchError: null })
    if (!formProblem(rows, units, t)) onChange(serialize(rows, units))
  }

  const toExpression = () => {
    // 一行都没填就切过去没问题（拼出来是空串）；有填错的行才拦下来——
    // 否则那几行会被静默丢掉。
    const current = firstRowProblem(state.rows, state.units, t)
    if (current) {
      setState({ ...state, switchError: current })
      return
    }
    const text = serialize(state.rows, state.units)
    setState({ ...state, mode: 'expr', text, switchError: null })
    onChange(text)
  }

  const toForm = () => {
    // 空串就是一张空表，不是「解析不出来的表达式」。
    if (!state.text.trim()) {
      setState({ ...state, mode: 'form', rows: EMPTY_ROWS, units: DEFAULT_UNITS, switchError: null })
      return
    }
    const parsed = parseSimpleExpr(state.text)
    if (!parsed) {
      const message = state.text.includes('?')
        ? t('ops.expr.conditionalOnly')
        : t('ops.expr.notSimple')
      setState({ ...state, switchError: message })
      return
    }
    setState({ ...state, mode: 'form', rows: parsed.rows, units: parsed.units, switchError: null })
    onChange(serialize(parsed.rows, parsed.units))
  }

  const setText = (text: string) => {
    setState({ ...state, text, switchError: null })
    onChange(text)
  }

  return (
    <div className="expr-editor">
      <div className="expr-modes">
        <div className="seg" role="group" aria-label={t('ops.expr.mode')}>
          <button type="button" aria-pressed={state.mode === 'form'} onClick={toForm}>
            {t('ops.expr.formMode')}
          </button>
          <button type="button" aria-pressed={state.mode === 'expr'} onClick={toExpression}>
            {t('ops.expr.exprMode')}
          </button>
        </div>
      </div>

      {state.mode === 'form' ? (
        <>
          <div className="expr-rows">
            {SLOTS.map((slot, index) => {
              const raw = state.rows[slot].trim()
              const token = raw ? toToken(raw, state.units[slot]) : null
              const invalid = raw !== '' && rowProblem(raw, state.units[slot], index + 1, t) !== null
              return (
                <div className="expr-row" key={slot}>
                  <label htmlFor={`${id}-${slot}`}>{t(LABELS[slot])}</label>
                  <input
                    id={`${id}-${slot}`}
                    className={invalid ? 'input invalid' : 'input'}
                    inputMode="decimal"
                    autoComplete="off"
                    aria-invalid={invalid}
                    value={state.rows[slot]}
                    onChange={(event) => setRow(slot, event.target.value, state.units[slot])}
                  />
                  <select
                    className="input"
                    aria-label={`${t(LABELS[slot])} ${t('ops.expr.unit')}`}
                    value={state.units[slot]}
                    onChange={(event) =>
                      setRow(slot, state.rows[slot], event.target.value as Unit)
                    }
                  >
                    <option value="million">{t('ops.expr.unitMillion')}</option>
                    <option value="thousand">{t('ops.expr.unitThousand')}</option>
                    <option value="token">{t('ops.expr.unitToken')}</option>
                  </select>
                  {token !== null && (
                    <span className="expr-derived">
                      {t('ops.expr.perToken')} {token} · {t('ops.expr.perMillion')}{' '}
                      {multiplyByPower(token, UNIT_SHIFT.million)}
                    </span>
                  )}
                </div>
              )
            })}
          </div>
          <p className="hint">{t('ops.expr.example')}</p>
        </>
      ) : (
        <div className="field">
          <label htmlFor={`${id}-expr`}>{t('ops.pricing.expr')}</label>
          <textarea
            id={`${id}-expr`}
            className="input mono"
            rows={5}
            value={state.text}
            onChange={(event) => setText(event.target.value)}
          />
        </div>
      )}

      {state.switchError && (
        <span className="hint error" role="alert">
          {state.switchError}
        </span>
      )}
    </div>
  )
}
