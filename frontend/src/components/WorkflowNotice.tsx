// WorkflowNotice: the ONE way workflow-guard warnings render (plan 2026-07-10) —
// a compact banner matching the existing Totals-banner style (ReviewTab.tsx:56),
// so hazards look identical on tabs, Overview, and dialogs. Copy comes from
// lib/warnings.tsx warningView (or bespoke call-site text); this component only
// owns tone + layout + the optional fix-it link.

import type { ReactNode } from "react";
import { Link } from "react-router";
import { cx } from "./ui";

export type WorkflowNoticeTone = "info" | "warning" | "danger";

const noticeTones: Record<WorkflowNoticeTone, string> = {
  info: "bg-neutral-50 text-neutral-700 ring-neutral-200",
  warning: "bg-amber-50 text-amber-800 ring-amber-200",
  danger: "bg-red-50 text-red-800 ring-red-200",
};

export function WorkflowNotice({
  tone,
  to,
  children,
}: {
  tone: WorkflowNoticeTone;
  /** Route that fixes the hazard (e.g. `/assessments/3?tab=identify`). */
  to?: string;
  children: ReactNode;
}) {
  return (
    <p className={cx("rounded-md px-3 py-2 text-xs ring-1 ring-inset", noticeTones[tone])}>
      {children}
      {to !== undefined && (
        <>
          {" "}
          <Link to={to} className="font-medium underline">
            Fix it →
          </Link>
        </>
      )}
    </p>
  );
}
