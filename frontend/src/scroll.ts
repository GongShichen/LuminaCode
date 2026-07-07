export function getPaneScroll(pane: any): number {
  const scroll = Number(pane.getScroll?.());
  if (Number.isFinite(scroll)) return Math.max(0, Math.floor(scroll));
  const childBase = Number(pane.childBase || 0);
  return Number.isFinite(childBase) ? Math.max(0, Math.floor(childBase)) : 0;
}

export function isPaneAtBottom(pane: any): boolean {
  const percent = Number(pane.getScrollPerc?.());
  if (Number.isFinite(percent)) return percent >= 99;
  const scroll = getPaneScroll(pane);
  const contentHeight = Number(pane.getScrollHeight?.() || 0);
  const visibleHeight = Math.max(1, Number(pane.height || 0) - 2);
  if (!Number.isFinite(contentHeight) || contentHeight <= visibleHeight) return true;
  return scroll + visibleHeight >= contentHeight - 1;
}
