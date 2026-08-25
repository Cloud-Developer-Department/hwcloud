// 内联 cn() 助手——静态站条件类场景少，不引入 clsx/tailwind-merge。
// 过滤 falsy 后用空格连接。如果后期条件类变多，加 clsx 是一行 package.json
// 的事，但不要预先装。
export function cn(...classes: (string | false | null | undefined)[]): string {
  return classes.filter(Boolean).join(" ");
}
