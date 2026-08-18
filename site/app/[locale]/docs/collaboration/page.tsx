import { setRequestLocale, getTranslations } from "next-intl/server";
import { CodeBlock } from "@/components/code-block";
import { Callout } from "@/components/callout";

const REPO = "https://github.com/yusheng-g/agentwork";

type Behavior = { name: string; desc: string; who: string; chips: string[]; example: string };

export default async function CollaborationPage({
  params,
}: {
  params: Promise<{ locale: string }>;
}) {
  const { locale } = await params;
  setRequestLocale(locale);
  const t = await getTranslations("docs.collab");
  const behaviors = t.raw("behaviors") as Behavior[];

  return (
    <article>
      <h1 className="mb-3 text-3xl font-bold text-text">{t("title")}</h1>
      <p className="mb-10 text-base leading-relaxed text-muted">{t("intro")}</p>

      {/* 四行为卡片网格 */}
      <div className="grid gap-4 md:grid-cols-2">
        {behaviors.map((b) => (
          <div
            key={b.name}
            className="flex flex-col rounded-xl border border-border bg-surface p-5 transition-colors hover:border-accent"
          >
            <div className="flex items-baseline justify-between">
              <h3 className="font-mono text-lg font-semibold text-accent">{b.name}</h3>
              {/* 权限 chip */}
              <span className="rounded-full border border-border bg-surface-2 px-2.5 py-0.5 font-mono text-[10px] text-muted">
                {b.who}
              </span>
            </div>
            <p className="mt-2 mb-3 text-sm leading-relaxed text-muted">{b.desc}</p>
            <div className="mb-3 flex flex-wrap gap-1.5">
              {b.chips.map((chip) => (
                <span
                  key={chip}
                  className="rounded-full border border-border bg-surface-2 px-2 py-0.5 font-mono text-[10px] text-accent"
                >
                  {chip}
                </span>
              ))}
            </div>
            <div className="mt-auto">
              <CodeBlock>{b.example}</CodeBlock>
            </div>
          </div>
        ))}
      </div>

      {/* token 身份说明 callout */}
      <div className="mt-10">
        <Callout>{t("tokenNote")}</Callout>
      </div>

      <p className="mt-8">
        <a
          href={`${REPO}/blob/master/Collaboration.v2.md`}
          target="_blank"
          rel="noopener noreferrer"
          className="text-sm font-medium text-accent transition-colors hover:text-accent-hover"
        >
          {t("fullModel")}
        </a>
      </p>
    </article>
  );
}
