// Icon inventory: inline SVGs, Lucide/Feather-style stroke paths. No icon library
// dependency — every icon here is a plain component so the bundle only ever ships
// the shapes actually used. 24x24 viewBox, stroke-based, rendered at 16px (Tailwind
// `size-4`) by default. Every icon is aria-hidden="true" — icons never carry an
// accessible name on their own; the control wrapping them (see IconButton in
// ./ui.tsx) is responsible for aria-label/title. See the binding design rules in
// docs/superpowers/plans/2026-07-17-icon-buttons.md.

import type { SVGProps } from "react";

export type IconProps = SVGProps<SVGSVGElement>;

// Spread AFTER {...props} in every icon so these defaults always win: the icon
// style contract (stroke geometry) and especially aria-hidden are binding —
// callers must not be able to clobber them via pass-through props.
const base: IconProps = {
  viewBox: "0 0 24 24",
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 2,
  strokeLinecap: "round",
  strokeLinejoin: "round",
  "aria-hidden": true,
};

/** Edit / rename. */
export function Pencil({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M12 20h9" />
      <path d="M16.5 3.5a2.121 2.121 0 0 1 3 3L7 19l-4 1 1-4Z" />
    </svg>
  );
}

/** Delete. */
export function Trash({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M3 6h18" />
      <path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
      <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
    </svg>
  );
}

/** Close / remove. */
export function X({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M18 6 6 18" />
      <path d="M6 6l12 12" />
    </svg>
  );
}

/** Archive (box). */
export function Archive({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <rect x="3" y="4" width="18" height="5" rx="1" />
      <path d="M5 9v9a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V9" />
      <path d="M10 13h4" />
    </svg>
  );
}

/** Paper-plane — resend / send. */
export function Send({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M22 2 11 13" />
      <path d="M22 2 15 22l-4-9-9-4Z" />
    </svg>
  );
}

/** Undo-style arrow — retract / reinstate-style rollback. */
export function RotateCcw({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M3 12a9 9 0 1 0 3-6.7" />
      <path d="M3 3v5h5" />
    </svg>
  );
}

/** Withdraw a person. */
export function UserMinus({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z" />
      <path d="M2 21v-2a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v2" />
      <path d="M17 11h6" />
    </svg>
  );
}

/** Reinstate a person. */
export function UserCheck({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z" />
      <path d="M2 21v-2a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v2" />
      <path d="m16 11 2 2 4-4" />
    </svg>
  );
}

/** Provider connectivity test. */
export function Zap({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M13 2 3 14h9l-1 8 10-12h-9l1-8Z" />
    </svg>
  );
}

/** Pricing. */
export function Tag({ className, ...props }: IconProps) {
  return (
    <svg {...props} {...base} className={className ?? "size-4"}>
      <path d="M12.586 2.586A2 2 0 0 0 11.172 2H4a2 2 0 0 0-2 2v7.172a2 2 0 0 0 .586 1.414l8 8a2 2 0 0 0 2.828 0l7.172-7.172a2 2 0 0 0 0-2.828z" />
      <circle cx="7.5" cy="7.5" r="1.5" />
    </svg>
  );
}
