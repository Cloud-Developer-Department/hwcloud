import { useTranslations } from "next-intl";

const REPO = "https://github.com/yusheng-g/hwcloud";

export function Footer() {
  const t = useTranslations("footer");
  return (
    <footer className="mt-20 border-t border-border py-8 text-sm text-muted">
      <div className="mx-auto flex max-w-7xl flex-col gap-4 px-6 sm:flex-row sm:justify-between">
        <div className="flex items-center gap-2">
          <span className="font-mono font-semibold text-text">hwcloud</span>
        </div>
        <div className="flex gap-6">
          <a href={REPO} className="transition-colors hover:text-accent">GitHub</a>
          <a href={`${REPO}/blob/master/LICENSE`} className="transition-colors hover:text-accent">
            {t("license")}
          </a>
        </div>
      </div>
    </footer>
  );
}
