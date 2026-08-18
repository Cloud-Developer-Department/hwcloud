import { defineRouting } from "next-intl/routing";

// 双语 locale 集合 + 默认。静态导出下不用 middleware 做 locale 检测/
// 重定向（middleware 与 output:'export' 不兼容）；改走显式 /en /zh 路由
// 分组 + generateStaticParams 枚举。
export const routing = defineRouting({
  locales: ["en", "zh"],
  defaultLocale: "en",
});

export type Locale = (typeof routing.locales)[number];
