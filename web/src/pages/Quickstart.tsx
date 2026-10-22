import { Link } from 'react-router-dom'
import { api } from '../api'
import { useI18n } from '../i18n'
import { useSite } from '../site/SiteProvider'
import { useAsync } from '../hooks/useAsync'
import { CopyButton, PageHead } from '../components/ui'
import { CodeCard, type CodeTab } from '../components/CodeCard'

export function Quickstart() {
  const { t, lang } = useI18n()
  const { meta } = useSite()
  const { data } = useAsync(() => api.models(), [])
  const base = meta.api_base_url.replace(/\/$/, '')
  // Anthropic SDK 自己会拼 `/v1/messages`，base_url 里不能再带一层 `/v1`。
  const anthropicBase = base.replace(/\/v1$/, '')
  const model = data?.items[0]?.name ?? 'example-model'
  const envComment = lang === 'zh' ? '从环境变量读，别写死' : 'read from env, never hardcode'

  const tabs: CodeTab[] = [
    {
      id: 'curl',
      label: t('quickstart.tabCurl'),
      language: 'bash',
      code: `# 用你的密钥调用
curl ${base}/chat/completions \\
  -H "Authorization: Bearer $LARKAX_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${model}","messages":[{"role":"user","content":"hi"}]}'`,
    },
    {
      id: 'openai',
      label: t('quickstart.tabOpenAI'),
      language: 'python',
      code: `from openai import OpenAI

client = OpenAI(
    base_url="${base}",
    api_key="<API_KEY>",  # ${envComment}
)

resp = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "hi"}],
)
print(resp.choices[0].message.content)`,
    },
    {
      id: 'anthropic',
      label: t('quickstart.tabAnthropic'),
      language: 'python',
      code: `from anthropic import Anthropic

client = Anthropic(
    base_url="${anthropicBase}",
    api_key="<API_KEY>",  # ${envComment}
)

message = client.messages.create(
    model="${model}",
    max_tokens=256,
    messages=[{"role": "user", "content": "hi"}],
)
print(message.content[0].text)`,
    },
  ]

  return (
    <>
      <PageHead title={t('quickstart.title')} subtitle={t('quickstart.subtitle')} />

      <section className="card panel">
        <div>
          <h2>{t('quickstart.baseUrl')}</h2>
          <p className="sub">{t('quickstart.baseUrlHint')}</p>
        </div>
        <div className="copyrow">
          <span className="copyfield mono">{base}</span>
          <CopyButton text={base} />
        </div>
      </section>

      <div className="stack-14">
        <div className="step">
          <span className="n">1</span>
          <div>
            <b style={{ fontSize: 13 }}>{t('quickstart.step1Title')}</b>
            <div className="tiny" style={{ marginTop: 2 }}>
              {t('quickstart.step1Body')}
            </div>
            <Link className="btn secondary sm" to="/app/keys" style={{ marginTop: 8 }}>
              {t('quickstart.step1Action')}
            </Link>
          </div>
        </div>
        <div className="step">
          <span className="n">2</span>
          <div>
            <b style={{ fontSize: 13 }}>{t('quickstart.step2Title')}</b>
            <div className="tiny" style={{ marginTop: 2 }}>
              {t('quickstart.step2Body')}
            </div>
            <pre className="codeblock" style={{ marginTop: 8 }}>
              <code>export LARKAX_API_KEY=&quot;&lt;API_KEY&gt;&quot;</code>
            </pre>
          </div>
        </div>
        <div className="step">
          <span className="n">3</span>
          <div>
            <b style={{ fontSize: 13 }}>{t('quickstart.step3Title')}</b>
            <div className="tiny" style={{ marginTop: 2 }}>
              {t('quickstart.step3Body')}
            </div>
          </div>
        </div>
      </div>

      <CodeCard tabs={tabs} />

      <section className="card table">
        <div className="thead endpoint-head">
          <span>{t('quickstart.endpoints')}</span>
          <span>{t('quickstart.protocol')}</span>
          <span>{t('quickstart.method')}</span>
        </div>
        {[
          ['/v1/chat/completions', 'OpenAI', 'POST'],
          ['/v1/messages', 'Anthropic', 'POST'],
          ['/v1/responses', 'OpenAI', 'POST'],
        ].map(([endpoint, protocol, method]) => (
          <div className="util-row endpoint-row" key={endpoint}>
            <span className="mono">{endpoint}</span>
            <span>{protocol}</span>
            <span>{method}</span>
          </div>
        ))}
      </section>
    </>
  )
}
