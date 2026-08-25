---
name: powerpoint
description: Create designed, editable PowerPoint .pptx presentations with PptxGenJS. Use when the user asks to create, generate, update, or inspect a deck, slide deck, presentation, or .pptx file.
---

# PowerPoint

Use this skill whenever a PowerPoint deck is involved. For new decks, pass a trusted PptxGenJS build script directly to the `pptx_write` tool. For filling or editing an existing template, call `pptx_template_analyze` first and then `pptx_template_fill` with the exact IDs returned by analysis.

## Workflow

1. Decide the deck outline and choose a visual system: palette, typography, repeated motif, and slide rhythm.
2. Write JavaScript module content that exports `default async function build(pptx, ctx)` or named `build(pptx, ctx)`.
3. In the script, add slides directly with PptxGenJS. Do not generate HTML for this workflow.
4. Call `pptx_write` with `path`, `script`, optional `assets_dir`, and optional `data`.
5. Verify the result with `pptx_read`; for visual QA, convert the PPTX to images if the environment has LibreOffice and Poppler.

## Template Workflow

- Use `pptx_template_analyze` when the user provides a `.pptx` template or wants to preserve existing layouts, charts, images, tables, or SmartArt.
- Build a `template_fill_pptx_plan.v1` plan from the returned slide IDs and object IDs, then call `pptx_template_fill`.
- For SmartArt, use `smartarts[*].smartart_id` and `smartarts[*].nodes[*].node_id` in `smartart_edits`. This edits existing node text only; it does not create, delete, or relayout SmartArt nodes.

## Script Creation

- Put the complete JavaScript module in the `script` argument.
- Do not use `local_file_write` or shell commands to create a temporary `.mjs` file for this workflow.
- If revising a deck, update the `script` content and call `pptx_write` again.

## Tool Contract

```json
{
  "tool": "pptx_write",
  "arguments": {
    "path": "deck.pptx",
    "script": "export default async function build(pptx, ctx) {\\n  pptx.layout = \"LAYOUT_WIDE\";\\n}",
    "assets_dir": "/absolute/path/to/assets",
    "data": {"title": "Quarterly Review"}
  }
}
```

The worker creates the PptxGenJS instance and writes the output file. The script only adds slides and content.

```javascript
export default async function build(pptx, ctx) {
  pptx.layout = "LAYOUT_WIDE";
  pptx.author = "OpenAgent";

  const slide = pptx.addSlide();
  slide.background = { color: "FFFFFF" };
  slide.addText("Title", {
    x: 0.6, y: 0.4, w: 8, h: 0.6,
    fontSize: 36, bold: true, color: "1F2937",
    margin: 0,
  });
  slide.addNotes("speaker notes");
}
```

`ctx` includes:

- `ctx.data`: JSON data passed from the tool call.
- `ctx.assetsDir`: resolved asset directory.
- `ctx.outPath`: final PPTX path.
- `ctx.resolveAsset("image.png")`: absolute path under `assets_dir`.
- `ctx.imageData("image.png")`: base64 image data URL.
- `ctx.iconSvgData("check", "16A34A")`: Font Awesome solid icon as SVG data.

## Design Rules

- **Set slide background explicitly on every slide.** PptxGenJS defaults to a black background; if you do not set `slide.background = { color: "FFFFFF" }`, slides will render with a black background. Always set the background color even for "white" slides.
- Avoid plain white bullet decks. Every slide should have a visual element: shape, image, chart, icon, timeline, stat callout, or diagram.
- Vary layouts across the deck: title, divider, two-column, card grid, process flow, quote/callout, and conclusion.
- Pick topic-specific colors. Use one dominant color, one or two supporting tones, and one accent.
- Use strong hierarchy: titles around 36-44 pt, section labels around 20-24 pt, body text around 14-18 pt.
- Keep at least 0.5 inch margins and consistent gaps around 0.3-0.5 inch.
- Use editable text wherever practical; use images for photos, screenshots, logos, or complex visual backgrounds.
- Add speaker notes when useful; `pptx_read` can surface them later.

## PptxGenJS Reference

Use this reference when writing the JavaScript build script for the `pptx_write` tool.

### Basic Structure

```javascript
export default async function build(pptx, ctx) {
  pptx.layout = "LAYOUT_WIDE";
  pptx.title = ctx.data?.title || "Presentation";

  const slide = pptx.addSlide();
  slide.background = { color: "FFFFFF" };
  slide.addText("Hello", { x: 0.6, y: 0.5, w: 8, h: 0.6, fontSize: 36 });
}
```

Useful layouts: `LAYOUT_WIDE` (13.333 x 7.5 in), `LAYOUT_16X9` (10 x 5.625 in), `LAYOUT_4X3` (10 x 7.5 in). Use inches for all `x`, `y`, `w`, `h` values.

### Text

```javascript
slide.addText("Main title", {
  x: 0.6, y: 0.4, w: 8.5, h: 0.6,
  fontFace: "Aptos Display", fontSize: 38, bold: true, color: "111827", margin: 0,
});

slide.addText([
  { text: "First point", options: { bullet: true, breakLine: true } },
  { text: "Second point", options: { bullet: true } },
], { x: 0.8, y: 1.4, w: 5.4, h: 1.2, fontSize: 17, color: "374151", paraSpaceAfterPt: 8 });
```

- Use `margin: 0` when aligning text with shapes or icons.
- Use PptxGenJS bullets; do not type bullet glyphs into strings.
- Use `breakLine: true` in rich text arrays when items must appear on separate lines.

### Shapes

```javascript
slide.addShape(pptx.ShapeType.rect, {
  x: 0, y: 0, w: 13.333, h: 7.5, fill: { color: "F8FAFC" }, line: { color: "F8FAFC" },
});

slide.addShape(pptx.ShapeType.roundRect, {
  x: 0.7, y: 1.2, w: 3.5, h: 1.2, rectRadius: 0.08, fill: { color: "FFFFFF" },
  line: { color: "E5E7EB", width: 1 },
  shadow: { type: "outer", color: "000000", opacity: 0.12, blur: 2, angle: 45, distance: 1 },
});
```

- Hex colors must not include `#`. Do not use 8-character hex for transparency — use `transparency` or `opacity`.
- Use a fresh options object for each shape; PptxGenJS mutates some option values internally.

### Images and Icons

```javascript
slide.addImage({ data: ctx.imageData("photo.png"), x: 7.1, y: 1.0, w: 5.4, h: 3.4,
  sizing: { type: "cover", x: 7.1, y: 1.0, w: 5.4, h: 3.4 } });

slide.addImage({ data: ctx.iconSvgData("chart-line", "2563EB"), x: 0.8, y: 1.2, w: 0.35, h: 0.35 });
```

- `ctx.resolveAsset()`: file path for PptxGenJS APIs that need a path.
- `ctx.imageData()`: base64 data URL for local PNG/JPG/GIF/SVG.
- `ctx.iconSvgData(name, color)`: Font Awesome **free-solid** icon as SVG. Only icons in `@fortawesome/free-solid-svg-icons` are available — using an icon name that does not exist in that package will fail. Safe icon names include: `check`, `check-circle`, `check-double`, `xmark`, `circle-xmark`, `triangle-exclamation`, `circle-info`, `circle-question`, `chart-line`, `chart-bar`, `chart-pie`, `arrow-right`, `arrow-up`, `arrow-down`, `arrow-left`, `chevron-right`, `chevron-up`, `chevron-down`, `plus`, `minus`, `star`, `heart`, `bolt`, `fire`, `lightbulb`, `gear`, `cog`, `wrench`, `code`, `terminal`, `database`, `server`, `cloud`, `shield-halved`, `lock`, `key`, `user`, `users`, `building`, `briefcase`, `rocket`, `target`, `flag`, `trophy`, `magnifying-glass`, `download`, `upload`, `trash`, `pen`, `clipboard`, `book`, `book-open`, `graduation-cap`, `certificate`, `id-card`, `envelope`, `phone`, `globe`, `link`, `clock`, `calendar`, `map`, `location-dot`, `camera`, `image`, `video`, `microphone`, `headphones`, `play`, `pause`, `stop`, `forward`, `backward`, `volume-high`, `wifi`, `plug`, `battery-full`, `bell`, `comment`, `comments`, `message`, `thumbs-up`, `thumbs-down`, `face-smile`, `face-frown`. When in doubt, use `circle-info` or omit the icon.

### Tables and Charts

```javascript
slide.addTable([["Metric", "Current", "Target"], ["Activation", "42%", "55%"]], {
  x: 0.7, y: 1.5, w: 6.0, h: 1.2, border: { pt: 1, color: "E5E7EB" }, fontSize: 12,
});

slide.addChart(pptx.ChartType.bar, [{ name: "Revenue", labels: ["Q1","Q2","Q3","Q4"], values: [12,16,21,28] }], {
  x: 0.7, y: 1.2, w: 6.2, h: 3.8, barDir: "col", chartColors: ["2563EB"], showValue: true,
});
```

### Layout Ideas

Cover (full color/image + large title), Agenda (sidebar + numbered sections), Two-column (text + visual), Card grid (2x2/3x2 with icon+header+body), Timeline/process (numbered steps), Data slide (large chart + stat callouts), Closing (statement/next action).

## Required QA

- Run `pptx_read` on the generated file and check slide order, missing text, typo risk, and notes.
- Inspect generated XML or render slides when visual precision matters.
- Watch for overlap, text overflow, low contrast, cramped spacing, repeated layouts, and leftover placeholder text.
- If a visual issue is found, edit the `.mjs` script and rewrite the PPTX.
