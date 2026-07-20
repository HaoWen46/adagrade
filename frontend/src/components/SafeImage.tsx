// <img> wrapper that swaps in a small retry card when loading fails (blob-store
// hiccup, dropped connection, …). Retry busts the browser cache with a throwaway
// query param. Failure state is keyed to the src, so changing src recovers.
//
// Layout stability: the img and the error card must occupy the exact same box,
// or a load/fail/swap reflows whatever sits below or beside it (clickable rows,
// keyboard-navigated strips — see ReviewStrip, SubmissionsTab). To get that,
// callers should pass a fixed-size `className` (e.g. "h-24 w-20 object-contain")
// — it's applied to both the <img> and the error card, and the error card fills
// that box (h-full w-full) instead of sizing itself off its own padding. For
// call sites that can't size in advance, pass `aspect` (a CSS aspect-ratio
// value like "3/4" or "auto 3/4") to reserve space without a fixed pixel size.

import { useState, type ImgHTMLAttributes } from "react";

export function SafeImage({
  src,
  alt,
  aspect,
  className,
  style,
  ...props
}: ImgHTMLAttributes<HTMLImageElement> & { src: string; aspect?: string }) {
  const [failedSrc, setFailedSrc] = useState<string | null>(null);
  const [bust, setBust] = useState<number | null>(null);
  const sizeStyle = aspect ? { ...style, aspectRatio: aspect } : style;

  if (failedSrc === src) {
    return (
      <div
        // Error cards can sit inside clickable rows / drag canvases — contain
        // interactions so Retry doesn't zoom, toggle, or draw behind itself.
        onClick={(e) => e.stopPropagation()}
        onPointerDown={(e) => e.stopPropagation()}
        className={`flex h-full w-full items-center justify-center gap-1.5 rounded-md border border-neutral-200 bg-neutral-50 text-sm text-neutral-500 ${className ?? ""}`}
        style={sizeStyle}
      >
        <span>Image failed to load —</span>
        <button
          type="button"
          onClick={() => {
            setBust(Date.now());
            setFailedSrc(null);
          }}
          className="font-medium text-indigo-600 hover:underline"
        >
          Retry
        </button>
      </div>
    );
  }

  const url = bust === null ? src : `${src}${src.includes("?") ? "&" : "?"}t=${bust}`;
  return (
    <img
      {...props}
      src={url}
      alt={alt}
      onError={() => setFailedSrc(src)}
      className={className}
      style={sizeStyle}
    />
  );
}
