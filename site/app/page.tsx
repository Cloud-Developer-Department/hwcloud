import { routing } from "@/i18n/routing";

// 根路径 / 不服务内容——跳到默认 locale。
// 静态导出（output:'export'）下 middleware 不可用，server redirect 在
// 纯静态产物里也不可靠。用一个纯客户端跳转页：meta http-equiv refresh
// 即时跳，noscript 里给一个手点链接兜底。
//
// basePath 注意：meta refresh 的 url 是字符串字面量，Next 不会自动加
// basePath 前缀（只有 <Link>/<Image>/useRouter 会）。用相对路径
// `${defaultLocale}/` 而非绝对 `/en/`——相对当前文档路径（gh-pages 上
// 是 /hwcloud/）解析成 /hwcloud/en/，本地 dev（/hwcloud/）也成立。
//
// 不再自己包 <html>/<body>——根 app/layout.tsx 已包（Next 要求唯一根
// layout 持有这两个标签，否则报 Missing <html> and <body> 错误）。
export default function RootPage() {
  const target = `${routing.defaultLocale}/`;
  return (
    <>
      <head>
        <meta httpEquiv="refresh" content={`0; url=${target}`} />
        <title>hwcloud</title>
      </head>
      <noscript>
        <a href={target}>hwcloud</a>
      </noscript>
    </>
  );
}
