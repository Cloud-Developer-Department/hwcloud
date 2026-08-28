import { setRequestLocale, getTranslations } from "next-intl/server";
import { CodeBlock } from "@/components/code-block";
import { Callout } from "@/components/callout";

type Step = { title: string; prompt: string; result: string };

export default async function IacServerPage({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  setRequestLocale(locale);
  const t = await getTranslations("docs.iac");

  const features = t.raw("features") as string[];
  const flow = t.raw("flow") as string[];
  const steps = t.raw("steps") as Step[];
  const paramRows = t.raw("paramRows") as { param: string; value: string; desc: string }[];
  const fieldRows = t.raw("fieldRows") as { field: string; loc: string; effect: string; miss: string }[];
  const notes = t.raw("notes") as string[];

  return (
    <article>
      <h1 className="mb-3 text-3xl font-bold text-text">{t("title")}</h1>
      <p className="mb-8 text-base leading-relaxed text-muted">{t("intro")}</p>

      {/* 什么是 IACMCPServer */}
      <section className="mb-12">
        <h2 className="mb-5 border-b border-border pb-2 text-lg font-semibold text-text">
          <span className="font-mono text-accent">#</span> {t("whatTitle")}
        </h2>
        <p className="mb-4 text-sm leading-relaxed text-muted">{t("whatDesc")}</p>
        <CodeBlock label="flow">{flow.join(" → ")}</CodeBlock>
        <p className="mb-4 mt-4 text-sm leading-relaxed text-muted">{t("whatClient")}</p>
        <CodeBlock label="opencode.json">{t("opencodeExample")}</CodeBlock>
        <p className="mb-3 mt-4 text-sm font-semibold text-text">{t("paramTitle")}</p>
        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-border text-left">
                <th className="py-2 pr-4 font-mono text-accent">PARAM</th>
                <th className="py-2 pr-4 font-mono text-accent">VALUE</th>
                <th className="py-2 text-muted">{t("paramColDesc")}</th>
              </tr>
            </thead>
            <tbody>
              {paramRows.map((r) => (
                <tr key={r.param} className="border-b border-border/50">
                  <td className="py-2 pr-4 font-mono text-text">{r.param}</td>
                  <td className="py-2 pr-4 font-mono text-code-text">{r.value}</td>
                  <td className="py-2 text-muted">{r.desc}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="mt-4 space-y-2">
          <p className="text-sm leading-relaxed text-muted">{t("urlInline")}</p>
          <ul className="list-disc space-y-1 pl-5 text-sm leading-relaxed text-muted">
            <li>{t("urlDevSpace")}</li>
            <li>{t("urlLocal")}</li>
          </ul>
        </div>
        <p className="mt-3 text-sm leading-relaxed text-muted">{t("urlSeeBelow")}</p>
      </section>

      {/* 它能做什么 */}
      <section className="mb-12">
        <h2 className="mb-5 border-b border-border pb-2 text-lg font-semibold text-text">
          <span className="font-mono text-accent">#</span> {t("capTitle")}
        </h2>
        <p className="mb-4 text-sm leading-relaxed text-muted">{t("capDesc")}</p>
        <ol className="mb-4 space-y-2 pl-5">
          {features.map((f, i) => (
            <li key={i} className="text-sm leading-relaxed text-muted">
              <span className="font-mono font-semibold text-accent">{i + 1}.</span> {f}
            </li>
          ))}
        </ol>
      </section>

      {/* 使用案例：部署一台最小 ECS */}
      <section className="mb-12">
        <h2 className="mb-5 border-b border-border pb-2 text-lg font-semibold text-text">
          <span className="font-mono text-accent">#</span> {t("caseTitle")}
        </h2>
        <p className="mb-6 text-sm leading-relaxed text-muted">{t("caseDesc")}</p>
        <div className="space-y-6">
          {steps.map((s, i) => (
            <div key={i} className="rounded-xl border border-border bg-surface p-5">
              <h3 className="mb-3 font-mono font-semibold text-accent">
                {t("stepLabel")} {i + 1}：{s.title}
              </h3>
              <CodeBlock label={t("youLabel")}>{s.prompt}</CodeBlock>
              <p className="my-2 text-xs text-muted">{t("serverReturns")}</p>
              <CodeBlock label={t("outLabel")}>{s.result}</CodeBlock>
            </div>
          ))}
        </div>
        <Callout type="warn" title={t("costWarnTitle")}>
          {t("costWarnBody")}
        </Callout>
      </section>

      {/* 连接 IACMCPServer */}
      <section className="mb-12">
        <h2 className="mb-5 border-b border-border pb-2 text-lg font-semibold text-text">
          <span className="font-mono text-accent">#</span> {t("connectTitle")}
        </h2>

        {/* 在开发者空间中直接使用 */}
        <h3 className="mb-3 mt-6 font-mono text-base font-semibold text-text">{t("devSpaceTitle")}</h3>
        <p className="mb-4 text-sm leading-relaxed text-muted">{t("devSpaceDesc")}</p>
        <p className="mb-3 text-sm leading-relaxed text-muted">{t("devSpaceOpencode")}</p>
        <CodeBlock label="opencode.json">{t("devSpaceExample")}</CodeBlock>
        <p className="mb-3 mt-4 text-sm font-semibold text-text">{t("fieldTitle")}</p>
        <div className="overflow-x-auto">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-border text-left">
                <th className="py-2 pr-4 font-mono text-accent">{t("fieldColField")}</th>
                <th className="py-2 pr-4 font-mono text-accent">{t("fieldColLoc")}</th>
                <th className="py-2 pr-4 text-muted">{t("fieldColEffect")}</th>
                <th className="py-2 text-muted">{t("fieldColMiss")}</th>
              </tr>
            </thead>
            <tbody>
              {fieldRows.map((r) => (
                <tr key={r.field} className="border-b border-border/50">
                  <td className="py-2 pr-4 font-mono text-text">{r.field}</td>
                  <td className="py-2 pr-4 font-mono text-code-text">{r.loc}</td>
                  <td className="py-2 pr-4 text-muted">{r.effect}</td>
                  <td className="py-2 text-muted">{r.miss}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <Callout type="info" title={t("toolCallNoteTitle")}>
          {t("toolCallNoteBody")}
        </Callout>
        <Callout type="warn" title={t("devSpaceWarnTitle")}>
          {t("devSpaceWarnBody")}
        </Callout>

        {/* 通过 DevBridge 暴露给外部设备 */}
        <h3 className="mb-3 mt-10 font-mono text-base font-semibold text-text">{t("bridgeTitle")}</h3>
        <p className="mb-6 text-sm leading-relaxed text-muted">{t("bridgeDesc")}</p>

        {/* 创建隧道 */}
        <h4 className="mb-2 mt-6 font-semibold text-text">{t("tunnelTitle")}</h4>
        <div className="space-y-3">
          <p className="text-sm leading-relaxed text-muted">
            <span className="font-mono font-semibold text-accent">1.</span> {t("tunnelStep1")}
          </p>
          <p className="text-sm leading-relaxed text-muted">
            <span className="font-mono font-semibold text-accent">2.</span> {t("tunnelStep2")}
          </p>
          <p className="text-sm leading-relaxed text-muted">
            <span className="font-mono font-semibold text-accent">3.</span> {t("tunnelStep3")}
          </p>
          <p className="text-sm leading-relaxed text-muted">
            <span className="font-mono font-semibold text-accent">4.</span> {t("tunnelStep4")}
          </p>
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-sm">
              <thead>
                <tr className="border-b border-border text-left">
                  <th className="py-2 pr-4 font-mono text-accent">{t("tunnelColConfig")}</th>
                  <th className="py-2 font-mono text-accent">{t("tunnelColValue")}</th>
                </tr>
              </thead>
              <tbody>
                <tr className="border-b border-border/50">
                  <td className="py-2 pr-4 font-mono text-text">{t("tunnelRowPortLabel")}</td>
                  <td className="py-2 font-mono text-code-text">{t("tunnelRowPortValue")}</td>
                </tr>
                <tr className="border-b border-border/50">
                  <td className="py-2 pr-4 font-mono text-text">{t("tunnelRowProtoLabel")}</td>
                  <td className="py-2 font-mono text-code-text">{t("tunnelRowProtoValue")}</td>
                </tr>
                <tr className="border-b border-border/50">
                  <td className="py-2 pr-4 font-mono text-text">{t("tunnelRowAnonLabel")}</td>
                  <td className="py-2 text-muted">{t("tunnelRowAnonValue")}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <Callout type="warn" title={t("anonWarnTitle")}>
            {t("anonWarnBody")}
          </Callout>
          <p className="text-sm leading-relaxed text-muted">
            <span className="font-mono font-semibold text-accent">5.</span> {t("tunnelStep5")}
          </p>
          <CodeBlock label="url">{t("tunnelUrl")}</CodeBlock>
          <p className="mt-2 text-sm leading-relaxed text-muted">{t("tunnelUrlDesc")}</p>
          <Callout type="info" title={t("modifyNoteTitle")}>
            {t("modifyNoteBody")}
          </Callout>
        </div>

        {/* 在 AI 工具中配置 MCP 连接 */}
        <h4 className="mb-2 mt-8 font-semibold text-text">{t("aiConfigTitle")}</h4>

        {/* 方式一：匿名访问 */}
        <h5 className="mb-2 mt-5 font-mono text-sm font-semibold text-accent">{t("methodAnonTitle")}</h5>
        <p className="mb-4 text-sm leading-relaxed text-muted">{t("methodAnonDesc")}</p>
        <CodeBlock label="opencode.json">{t("methodAnonExample")}</CodeBlock>
        <p className="mt-2 text-sm leading-relaxed text-muted">{t("methodAnonNote")}</p>

        {/* 方式二：需认证 */}
        <h5 className="mb-2 mt-6 font-mono text-sm font-semibold text-accent">{t("methodAuthTitle")}</h5>
        <p className="mb-4 text-sm leading-relaxed text-muted">{t("methodAuthDesc")}</p>
        <Callout type="info" title={t("aiInstallHintTitle")}>
          {t("aiInstallHintBody")}
        </Callout>

        <div className="space-y-6">
          {/* Step 1: install CLI */}
          <div className="rounded-xl border border-border bg-surface p-5">
            <h6 className="mb-3 font-mono font-semibold text-accent">{t("authStep1Title")}</h6>
            <p className="mb-3 text-sm leading-relaxed text-muted">{t("authStep1Desc")}</p>
            <CodeBlock label="bash">{t("authStep1Cmd")}</CodeBlock>
            <p className="mb-2 mt-3 text-sm text-muted">{t("authStep1Path")}</p>
            <p className="mb-1 text-sm font-semibold text-text">{t("authStep1TempTitle")}</p>
            <CodeBlock label="bash">{t("authStep1Temp")}</CodeBlock>
            <p className="mb-1 mt-3 text-sm font-semibold text-text">{t("authStep1PermTitle")}</p>
            <p className="mb-1 text-sm text-muted">{t("authStep1PermLinux")}</p>
            <CodeBlock label="bash">{t("authStep1PermLinuxCmd")}</CodeBlock>
            <p className="mb-1 mt-2 text-sm text-muted">{t("authStep1PermMac")}</p>
            <CodeBlock label="bash">{t("authStep1PermMacCmd")}</CodeBlock>
            <p className="mb-1 mt-2 text-sm text-muted">{t("authStep1PermWin")}</p>
            <CodeBlock label="powershell">{t("authStep1PermWinCmd")}</CodeBlock>
            <Callout type="info" title={t("winNoteTitle")}>
              {t("winNoteBody")}
            </Callout>
            <Callout type="info" title={t("winCurlTitle")}>
              {t("winCurlBody")}
            </Callout>
            <p className="mb-1 mt-3 text-sm text-muted">{t("authStep1Verify")}</p>
            <CodeBlock label="bash">{t("authStep1VerifyCmd")}</CodeBlock>
          </div>

          {/* Step 2: AK/SK */}
          <div className="rounded-xl border border-border bg-surface p-5">
            <h6 className="mb-3 font-mono font-semibold text-accent">{t("authStep2Title")}</h6>
            <p className="mb-2 text-sm leading-relaxed text-muted">{t("authStep2Desc")}</p>
            <p className="mb-1 text-sm font-semibold text-text">{t("authStep2PathTitle")}</p>
            <p className="text-sm leading-relaxed text-muted">{t("authStep2Path1")}</p>
            <p className="text-sm leading-relaxed text-muted">{t("authStep2Path2")}</p>
            <p className="text-sm leading-relaxed text-muted">{t("authStep2Path3")}</p>
            <Callout type="warn" title={t("skWarnTitle")}>
              {t("skWarnBody")}
            </Callout>
          </div>

          {/* Step 3: login */}
          <div className="rounded-xl border border-border bg-surface p-5">
            <h6 className="mb-3 font-mono font-semibold text-accent">{t("authStep3Title")}</h6>
            <CodeBlock label="bash">{t("authStep3Cmd")}</CodeBlock>
            <p className="mb-2 mt-3 text-sm text-muted">{t("authStep3Verify")}</p>
            <CodeBlock label="bash">{t("authStep3VerifyCmd")}</CodeBlock>
          </div>

          {/* Step 4: connect tunnel */}
          <div className="rounded-xl border border-border bg-surface p-5">
            <h6 className="mb-3 font-mono font-semibold text-accent">{t("authStep4Title")}</h6>
            <p className="mb-3 text-sm leading-relaxed text-muted">{t("authStep4Desc")}</p>
            <CodeBlock label="bash">{t("authStep4Cmd")}</CodeBlock>
            <p className="mb-2 mt-3 text-sm text-muted">{t("authStep4Desc2")}</p>
            <CodeBlock label="out">{t("authStep4Out")}</CodeBlock>
            <p className="mt-2 text-sm leading-relaxed text-muted">{t("authStep4Note")}</p>
            <Callout type="info" title={t("connectNoteTitle")}>
              {t("connectNoteBody")}
            </Callout>
          </div>

          {/* Step 5: configure AI tool */}
          <div className="rounded-xl border border-border bg-surface p-5">
            <h6 className="mb-3 font-mono font-semibold text-accent">{t("authStep5Title")}</h6>
            <CodeBlock label="opencode.json">{t("authStep5Example")}</CodeBlock>
            <p className="mt-2 text-sm leading-relaxed text-muted">{t("authStep5Note")}</p>
          </div>
        </div>

        {/* 流程图 */}
        <h6 className="mb-2 mt-8 font-mono text-sm font-semibold text-accent">{t("flowDiagramTitle")}</h6>
        <CodeBlock>{t("flowDiagram")}</CodeBlock>
      </section>

      {/* 注意事项 */}
      <section className="mb-12">
        <h2 className="mb-5 border-b border-border pb-2 text-lg font-semibold text-text">
          <span className="font-mono text-accent">#</span> {t("notesTitle")}
        </h2>
        <ol className="space-y-2 pl-5">
          {notes.map((n, i) => (
            <li key={i} className="text-sm leading-relaxed text-muted">
              <span className="font-mono font-semibold text-accent">{i + 1}.</span> {n}
            </li>
          ))}
        </ol>
      </section>
    </article>
  );
}
