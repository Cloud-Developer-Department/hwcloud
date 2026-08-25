import { DocSidebar } from "@/components/doc-sidebar";

// docs 子树共享布局：左侧暗色 sticky 侧边栏（DocSidebar）+
// 右侧内容区（max-width 对齐 main 宽度）。
export default function DocsLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <div className="mx-auto flex max-w-7xl">
      <DocSidebar />
      <div className="min-w-0 flex-1 px-8 py-12">{children}</div>
    </div>
  );
}
