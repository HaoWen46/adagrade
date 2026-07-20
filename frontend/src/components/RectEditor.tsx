// Generic drag-to-create / drag-to-move / corner-handle-resize editor for normalized
// (0..1) rectangles, rendered as absolutely-positioned divs over an image. Extracted
// from MaskingTab's region editor — this component knows nothing about what a rect
// *means* (mask region, ID region, …); callers own that mapping and any per-rect
// styling via `rectClassName` / `rectStyle`.

import { useRef, type CSSProperties, type PointerEvent as ReactPointerEvent } from "react";
import { cx } from "./ui";
import { SafeImage } from "./SafeImage";

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

interface Drag {
  mode: "create" | "move" | "resize";
  index: number;
  start: { x: number; y: number };
  orig: Rect;
}

export const clamp = (v: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, v));
export const pct = (v: number) => `${v * 100}%`;

export function RectEditor<R extends Rect>({
  rects,
  onChange,
  imageUrl,
  imageAlt = "Editable page",
  selectedIndex = null,
  onSelect,
  newRect,
  rectClassName,
  rectStyle,
  className,
}: {
  rects: R[];
  onChange: (rects: R[]) => void;
  imageUrl: string;
  imageAlt?: string;
  selectedIndex?: number | null;
  onSelect?: (index: number | null) => void;
  /** Extra fields to stamp onto a rect created by drawing on the background. */
  newRect: Omit<R, "x" | "y" | "w" | "h">;
  rectClassName?: (rect: R, index: number, selected: boolean) => string | undefined;
  rectStyle?: (rect: R, index: number, selected: boolean) => CSSProperties | undefined;
  className?: string;
}) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const dragRef = useRef<Drag | null>(null);

  const sel = selectedIndex !== null && selectedIndex < rects.length ? selectedIndex : null;

  const pointOf = (e: ReactPointerEvent) => {
    const rect = wrapRef.current!.getBoundingClientRect();
    return {
      x: clamp((e.clientX - rect.left) / rect.width, 0, 1),
      y: clamp((e.clientY - rect.top) / rect.height, 0, 1),
    };
  };

  const beginDrag = (e: ReactPointerEvent, drag: Drag) => {
    e.stopPropagation();
    if (e.button !== 0) return;
    dragRef.current = drag;
    onSelect?.(drag.index);
    wrapRef.current?.setPointerCapture(e.pointerId);
  };

  const onBackgroundDown = (e: ReactPointerEvent) => {
    if (e.button !== 0) return;
    const p = pointOf(e);
    const next = { ...newRect, x: p.x, y: p.y, w: 0, h: 0 } as R;
    beginDrag(e, { mode: "create", index: rects.length, start: p, orig: next });
    onChange([...rects, next]);
  };

  const onMove = (e: ReactPointerEvent) => {
    const drag = dragRef.current;
    if (!drag) return;
    const p = pointOf(e);
    onChange(
      rects.map((r, i) => {
        if (i !== drag.index) return r;
        switch (drag.mode) {
          case "create":
            return {
              ...r,
              x: Math.min(drag.start.x, p.x),
              y: Math.min(drag.start.y, p.y),
              w: Math.abs(p.x - drag.start.x),
              h: Math.abs(p.y - drag.start.y),
            };
          case "move":
            return {
              ...r,
              x: clamp(drag.orig.x + (p.x - drag.start.x), 0, 1 - drag.orig.w),
              y: clamp(drag.orig.y + (p.y - drag.start.y), 0, 1 - drag.orig.h),
            };
          case "resize":
            return {
              ...r,
              w: clamp(p.x - drag.orig.x, 0.01, 1 - drag.orig.x),
              h: clamp(p.y - drag.orig.y, 0.01, 1 - drag.orig.y),
            };
        }
      }),
    );
  };

  const onUp = () => {
    const drag = dragRef.current;
    dragRef.current = null;
    if (drag?.mode === "create") {
      // A bare click (no real drag) creates nothing.
      onChange(rects.filter((r, i) => i !== drag.index || (r.w >= 0.005 && r.h >= 0.005)));
    }
  };

  return (
    <div
      ref={wrapRef}
      onPointerDown={onBackgroundDown}
      onPointerMove={onMove}
      onPointerUp={onUp}
      onPointerCancel={onUp}
      className={cx(
        "relative max-w-xl cursor-crosshair touch-none overflow-hidden rounded-md border border-neutral-300 select-none",
        className,
      )}
    >
      <SafeImage
        src={imageUrl}
        alt={imageAlt}
        draggable={false}
        className="pointer-events-none block w-full"
      />
      {rects.map((r, i) => (
        <div
          key={i}
          onPointerDown={(e) => beginDrag(e, { mode: "move", index: i, start: pointOf(e), orig: r })}
          className={cx(
            "absolute cursor-move border",
            sel === i ? "border-indigo-600" : "border-neutral-500/60",
            rectClassName?.(r, i, sel === i),
          )}
          style={{
            left: pct(r.x),
            top: pct(r.y),
            width: pct(r.w),
            height: pct(r.h),
            ...rectStyle?.(r, i, sel === i),
          }}
        >
          <div
            onPointerDown={(e) =>
              beginDrag(e, { mode: "resize", index: i, start: pointOf(e), orig: r })
            }
            className="absolute -right-1 -bottom-1 size-3 cursor-nwse-resize rounded-sm border border-white bg-indigo-600"
          />
        </div>
      ))}
    </div>
  );
}
