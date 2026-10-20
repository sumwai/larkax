import { useMemo, useState } from 'react'
import { useI18n } from '../i18n'
import { copyText } from './ui'
import { IconCheck, IconCopy } from './icons'

export interface CodeTab {
  id: string
  label: string
  language: 'bash' | 'python' | 'json'
  code: string
}

type Token = { text: string; cls?: string }

/**
 * 只够区分示例里的字符串、注释、数字、关键字与变量。
 * 不追求完整语法——代码块用来看懂，不是编辑器。
 */
function tokenize(code: string, language: CodeTab['language']): Token[] {
  const keyword =
    language === 'json'
      ? 'true|false|null'
      : language === 'bash'
        ? 'curl'
        : 'from|import|def|with|as|return|print|True|False|None|const|let|new'
  const pattern = new RegExp(
    [
      '("(?:[^"\\\\]|\\\\.)*"|\'(?:[^\'\\\\]|\\\\.)*\')', // 字符串
      '(#[^\\n]*)', // 注释
      '(\\$[A-Za-z_][A-Za-z0-9_]*)', // shell 变量
      '(\\b\\d+(?:\\.\\d+)?\\b)', // 数字
      `(\\b(?:${keyword})\\b)`, // 关键字
    ].join('|'),
    'g',
  )

  const tokens: Token[] = []
  let last = 0
  for (const match of code.matchAll(pattern)) {
    const index = match.index ?? 0
    if (index > last) tokens.push({ text: code.slice(last, index) })
    const text = match[0]
    let cls: string | undefined
    if (match[1]) {
      const rest = code.slice(index + text.length)
      cls = /^\s*:/.test(rest) ? 'tok-key' : 'tok-str'
    } else if (match[2]) cls = 'tok-cmt'
    else if (match[3]) cls = 'tok-var'
    else if (match[4]) cls = 'tok-num'
    else cls = 'tok-kw'
    tokens.push({ text, cls })
    last = index + text.length
  }
  if (last < code.length) tokens.push({ text: code.slice(last) })
  return tokens
}

function Highlight({ code, language }: { code: string; language: CodeTab['language'] }) {
  const tokens = useMemo(() => tokenize(code, language), [code, language])
  return (
    <>
      {tokens.map((token, index) =>
        token.cls ? (
          <span key={index} className={token.cls}>
            {token.text}
          </span>
        ) : (
          <span key={index}>{token.text}</span>
        ),
      )}
    </>
  )
}

export function CodeCard({ tabs }: { tabs: CodeTab[] }) {
  const { t } = useI18n()
  const [selected, setSelected] = useState(tabs[0]?.id ?? '')
  const [copied, setCopied] = useState(false)
  const active = tabs.find((tab) => tab.id === selected) ?? tabs[0]

  const copy = async () => {
    if (!active) return
    const ok = await copyText(active.code)
    if (!ok) return
    setCopied(true)
    window.setTimeout(() => setCopied(false), 2000)
  }

  if (!active) return null

  return (
    <div className="code-card">
      <div className="tabs" role="tablist" aria-label={t('quickstart.endpoints')}>
        {tabs.map((tab) => (
          <button
            key={tab.id}
            role="tab"
            aria-selected={tab.id === active.id}
            onClick={() => setSelected(tab.id)}
          >
            {tab.label}
          </button>
        ))}
      </div>
      <div className="code-panel">
        <button className="code-copy" onClick={copy} aria-label={t('common.copy')}>
          {copied ? <IconCheck size={12} /> : <IconCopy size={12} />}
          {copied ? t('common.copied') : t('common.copy')}
        </button>
        <pre className="codeblock" data-lang={active.language}>
          <code>
            <Highlight code={active.code} language={active.language} />
          </code>
        </pre>
      </div>
    </div>
  )
}
