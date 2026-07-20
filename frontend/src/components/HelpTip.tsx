// Inline "?" help affordance: click opens a dialog with a plain-language
// explanation (audience: course staff with no LLM background). Body copy lives
// in src/lib/helpContent.tsx so pages share one source of truth.

import { useState, type ReactNode } from "react";
import { Dialog } from "./ui";

export function HelpTip({ title, children }: { title: string; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button
        type="button"
        aria-label={`Help: ${title}`}
        onClick={(e) => {
          // HelpTips sit inside <label> wrappers and clickable rows: don't focus
          // the field or toggle the row, just open the help dialog.
          e.preventDefault();
          e.stopPropagation();
          setOpen(true);
        }}
        className="inline-flex size-3.5 shrink-0 cursor-help items-center justify-center rounded-full align-[-0.125em] text-[10px] font-semibold text-neutral-400 ring-1 ring-neutral-300 transition-colors ring-inset hover:text-indigo-600 hover:ring-indigo-400"
      >
        ?
      </button>
      {/* Kept mounted: the open prop drives showModal()/close() so the dialog is
          always properly closed before it could ever unmount (Safari inert-page bug). */}
      <Dialog open={open} onClose={() => setOpen(false)} title={title}>
        <div className="space-y-2 text-sm leading-relaxed text-neutral-600">{children}</div>
      </Dialog>
    </>
  );
}
