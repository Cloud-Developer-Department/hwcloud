import type { NextConfig } from "next";
import createNextIntlPlugin from "next-intl/plugin";

// next-intl 的 static-export 模式：plugin 包装 next config，但不用
// middleware（middleware 与 output:'export' 不兼容）。locale 路由走静态
// [locale] 分组 + generateStaticParams，i18n/request.ts 在每个请求/导出
// 时解析 locale → messages。
const withNextIntl = createNextIntlPlugin("./i18n/request.ts");

const nextConfig: NextConfig = {
  // 静态导出到 site/out/，gh-pages 直接服这个目录。
  output: "export",
  // 站点服在 Cloud-Developer-Department.github.io/hwcloud（project path，非 user root）。
  // <Link>/<Image>/useRouter 自动加此前缀；裸 <a> 和 CSS url() 不会——
  // 内部跳转一律走 <Link>，外部链接用裸 <a>。
  basePath: "/hwcloud",
  // gh-pages 从目录服 index.html：/docs/concepts/ → docs/concepts/index.html。
  // 无 trailingSlash 会找不到无扩展名的 URL。
  trailingSlash: true,
  // 静态导出不跑 Image Optimization server，必须关。
  images: { unoptimized: true },
};

export default withNextIntl(nextConfig);
