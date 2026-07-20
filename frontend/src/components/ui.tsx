// Hand-rolled UI primitives (no external component/icon deps).
// Clean, dense, utilitarian admin aesthetic: neutral grays + one indigo accent.

import {
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TdHTMLAttributes,
  type TextareaHTMLAttributes,
  type ThHTMLAttributes,
} from "react";

export function cx(...parts: Array<string | false | null | undefined>): string {
  return parts.filter(Boolean).join(" ");
}

// ---------------------------------------------------------------------------
// Button

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";

const buttonVariants: Record<ButtonVariant, string> = {
  primary:
    "bg-indigo-600 text-white hover:bg-indigo-500 focus-visible:outline-indigo-600",
  secondary:
    "border border-neutral-300 bg-white text-neutral-800 hover:bg-neutral-50 focus-visible:outline-neutral-400",
  ghost:
    "text-neutral-600 hover:bg-neutral-100 hover:text-neutral-900 focus-visible:outline-neutral-400",
  danger: "bg-red-600 text-white hover:bg-red-500 focus-visible:outline-red-600",
};

/** Shared class string so links can look like buttons (e.g. /auth/login). */
export function buttonClassName(variant: ButtonVariant = "primary", className?: string): string {
  return cx(
    "inline-flex items-center justify-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 disabled:pointer-events-none disabled:opacity-50",
    buttonVariants[variant],
    className,
  );
}

export function Button({
  variant = "primary",
  type = "button",
  className,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: ButtonVariant }) {
  return <button type={type} className={buttonClassName(variant, className)} {...props} />;
}

// ---------------------------------------------------------------------------
// IconButton
//
// Square, icon-only action button for repeated row-level actions where the
// column position already carries the meaning (see the binding design rules
// in docs/superpowers/plans/2026-07-17-icon-buttons.md). `label` is required
// and becomes BOTH the aria-label and the title-attr tooltip — an icon alone
// has no accessible name, so this must never be omitted. Sibling of Button:
// same focus-visible ring and disabled treatment.

export type IconButtonVariant = "default" | "danger";

const iconButtonVariants: Record<IconButtonVariant, string> = {
  default:
    "text-neutral-500 hover:bg-neutral-100 hover:text-neutral-900 focus-visible:outline-neutral-400",
  danger: "text-neutral-500 hover:bg-red-50 hover:text-red-600 focus-visible:outline-red-600",
};

export function IconButton({
  label,
  variant = "default",
  type = "button",
  className,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  /** Required — becomes both aria-label and the title tooltip. Never omit. */
  label: string;
  variant?: IconButtonVariant;
}) {
  return (
    // {...props} is spread FIRST so the mandatory label contract always wins:
    // a caller passing title/aria-label through props cannot clobber it.
    <button
      type={type}
      {...props}
      aria-label={label}
      title={label}
      className={cx(
        "inline-flex items-center justify-center rounded-md p-1.5 transition-colors focus-visible:outline-2 focus-visible:outline-offset-2 disabled:pointer-events-none disabled:opacity-50",
        iconButtonVariants[variant],
        className,
      )}
    />
  );
}

// ---------------------------------------------------------------------------
// Input

export function Input({ className, ...props }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cx(
        "w-full rounded-md border border-neutral-300 bg-white px-2.5 py-1.5 text-sm text-neutral-900 placeholder:text-neutral-400",
        "focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20",
        "disabled:cursor-not-allowed disabled:bg-neutral-100",
        className,
      )}
      {...props}
    />
  );
}

// ---------------------------------------------------------------------------
// Textarea

export function Textarea({ className, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return (
    <textarea
      className={cx(
        "w-full rounded-md border border-neutral-300 bg-white px-2.5 py-1.5 text-sm text-neutral-900 placeholder:text-neutral-400",
        "focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20",
        "disabled:cursor-not-allowed disabled:bg-neutral-100",
        className,
      )}
      {...props}
    />
  );
}

// ---------------------------------------------------------------------------
// Field (label wrapper for form controls)

export function Field({
  label,
  className,
  children,
}: {
  label: ReactNode;
  className?: string;
  children: ReactNode;
}) {
  return (
    <label className={cx("block", className)}>
      <span className="mb-1 block text-xs font-medium text-neutral-600">{label}</span>
      {children}
    </label>
  );
}

// ---------------------------------------------------------------------------
// Card

export function Card({
  title,
  actions,
  className,
  children,
}: {
  title?: ReactNode;
  actions?: ReactNode;
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className={cx("rounded-lg border border-neutral-200 bg-white shadow-sm", className)}>
      {(title !== undefined || actions !== undefined) && (
        <div className="flex items-center justify-between gap-2 border-b border-neutral-200 px-4 py-2.5">
          {title !== undefined ? (
            <h2 className="text-sm font-semibold text-neutral-900">{title}</h2>
          ) : (
            <span />
          )}
          {actions}
        </div>
      )}
      <div className="p-4">{children}</div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Table (compositional: <Table><thead><tr><TH/>… + <tbody><tr><TD/>…)

export function Table({ className, children }: { className?: string; children: ReactNode }) {
  return (
    <div className="overflow-x-auto rounded-lg border border-neutral-200 bg-white shadow-sm">
      <table className={cx("w-full border-collapse text-left text-sm", className)}>
        {children}
      </table>
    </div>
  );
}

export function TH({ className, ...props }: ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <th
      className={cx(
        "border-b border-neutral-200 bg-neutral-50 px-3 py-2 text-xs font-semibold tracking-wide text-neutral-500 uppercase",
        className,
      )}
      {...props}
    />
  );
}

export function TD({ className, ...props }: TdHTMLAttributes<HTMLTableCellElement>) {
  return (
    <td
      className={cx("border-b border-neutral-100 px-3 py-2 align-middle text-neutral-800", className)}
      {...props}
    />
  );
}

// ---------------------------------------------------------------------------
// Badge

export type BadgeTone = "neutral" | "indigo" | "green" | "amber" | "red";

const badgeTones: Record<BadgeTone, string> = {
  neutral: "bg-neutral-100 text-neutral-700 ring-neutral-200",
  indigo: "bg-indigo-50 text-indigo-700 ring-indigo-200",
  green: "bg-green-50 text-green-700 ring-green-200",
  amber: "bg-amber-50 text-amber-800 ring-amber-200",
  red: "bg-red-50 text-red-700 ring-red-200",
};

export function Badge({
  tone = "neutral",
  className,
  children,
}: {
  tone?: BadgeTone;
  className?: string;
  children: ReactNode;
}) {
  return (
    <span
      className={cx(
        "inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium ring-1 ring-inset",
        badgeTones[tone],
        className,
      )}
    >
      {children}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Dialog (native <dialog>; Esc and backdrop-click both close via onClose)

export function Dialog({
  open,
  onClose,
  title,
  className,
  children,
}: {
  open: boolean;
  onClose: () => void;
  title?: ReactNode;
  className?: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
    // Unmounting a still-open modal dialog without close() strands the page in an
    // inert/blocked state on some browsers (notably Safari) — always close first.
    return () => {
      if (el.open) el.close();
    };
  }, [open]);

  return (
    <dialog
      ref={ref}
      onClose={onClose}
      onClick={(e) => {
        // The dialog element itself is only the click target on the backdrop:
        // the inner divs cover the whole content box (p-0 on the dialog).
        if (e.target === ref.current) onClose();
      }}
      className={cx(
        "m-auto w-full max-w-md rounded-lg border border-neutral-200 bg-white p-0 shadow-xl backdrop:bg-neutral-950/40",
        className,
      )}
    >
      <div className="flex items-center justify-between gap-2 border-b border-neutral-200 px-4 py-2.5">
        <h2 className="text-sm font-semibold text-neutral-900">{title}</h2>
        <button
          type="button"
          aria-label="Close"
          onClick={onClose}
          className="rounded p-1 text-neutral-400 hover:bg-neutral-100 hover:text-neutral-700"
        >
          <svg viewBox="0 0 20 20" fill="none" className="size-4" aria-hidden="true">
            <path
              d="M5 5l10 10M15 5L5 15"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
          </svg>
        </button>
      </div>
      <div className="p-4">{children}</div>
    </dialog>
  );
}

// ---------------------------------------------------------------------------
// Select (styled native select with an inline chevron)

export function Select({
  className,
  children,
  ...props
}: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <span className={cx("relative inline-block", className)}>
      <select
        className={cx(
          "w-full appearance-none rounded-md border border-neutral-300 bg-white py-1.5 pr-8 pl-2.5 text-sm text-neutral-900",
          "focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20",
          "disabled:cursor-not-allowed disabled:bg-neutral-100",
        )}
        {...props}
      >
        {children}
      </select>
      <svg
        viewBox="0 0 20 20"
        fill="none"
        aria-hidden="true"
        className="pointer-events-none absolute top-1/2 right-2 size-4 -translate-y-1/2 text-neutral-400"
      >
        <path
          d="M6 8l4 4 4-4"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    </span>
  );
}

// ---------------------------------------------------------------------------
// Disclosure (native <details>/<summary>: keyboard accessible for free, with an
// explicit chevron affordance instead of the platform marker)

/** Collapsible section. `defaultOpen` is the INITIAL state only — the user's toggle
 * always wins afterwards. To force a reset (e.g. collapse once an item is resolved),
 * remount with a changed `key`. */
export function Disclosure({
  summary,
  defaultOpen = false,
  className,
  children,
}: {
  summary: ReactNode;
  defaultOpen?: boolean;
  className?: string;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <details
      open={open}
      onToggle={(e) => setOpen(e.currentTarget.open)}
      className={cx("group rounded-md border border-neutral-200 bg-white", className)}
    >
      <summary className="flex cursor-pointer list-none items-center gap-1.5 px-3 py-2 text-xs font-medium text-neutral-600 select-none hover:bg-neutral-50 [&::-webkit-details-marker]:hidden">
        <svg
          viewBox="0 0 20 20"
          fill="none"
          aria-hidden="true"
          className="size-3.5 shrink-0 text-neutral-400 transition-transform group-open:rotate-90"
        >
          <path
            d="M7 5l6 5-6 5"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
        <span className="min-w-0 flex-1">{summary}</span>
      </summary>
      <div className="border-t border-neutral-200 px-3 py-2.5">{children}</div>
    </details>
  );
}

// ---------------------------------------------------------------------------
// Spinner

export function Spinner({ className }: { className?: string }) {
  return (
    <span
      role="status"
      aria-label="Loading"
      className={cx(
        "inline-block size-4 animate-spin rounded-full border-2 border-neutral-300 border-t-indigo-600",
        className,
      )}
    />
  );
}

// ---------------------------------------------------------------------------
// Pager

/** pageWindow picks which page numbers a Pager shows: everything when short,
    else first/last/current±1 with null gaps rendered as ellipses. */
function pageWindow(page: number, pageCount: number): Array<number | null> {
  if (pageCount <= 7) {
    return Array.from({ length: pageCount }, (_, i) => i);
  }
  const shown = [...new Set([0, page - 1, page, page + 1, pageCount - 1])]
    .filter((p) => p >= 0 && p < pageCount)
    .sort((a, b) => a - b);
  const out: Array<number | null> = [];
  shown.forEach((p, i) => {
    if (i > 0 && p - shown[i - 1] > 1) out.push(null);
    out.push(p);
  });
  return out;
}

/** Pager: arrow buttons plus a clickable page-number window (1 … 4 5 6 … 12),
    so any page — and therefore any student — is a couple of clicks away
    instead of a Next-spam. `page` is 0-based; hidden entirely for one page. */
export function Pager({
  page,
  pageCount,
  onPage,
  className,
}: {
  page: number;
  pageCount: number;
  onPage: (page: number) => void;
  className?: string;
}) {
  if (pageCount <= 1) return null;
  return (
    <nav aria-label="Pagination" className={cx("flex items-center gap-1", className)}>
      <Button
        variant="secondary"
        className="px-2 py-1 text-xs"
        aria-label="Previous page"
        title="Previous page"
        disabled={page <= 0}
        onClick={() => onPage(page - 1)}
      >
        ←
      </Button>
      {pageWindow(page, pageCount).map((p, i) =>
        p === null ? (
          <span key={`gap-${i}`} className="px-0.5 text-xs text-neutral-400">
            …
          </span>
        ) : (
          <Button
            key={p}
            variant={p === page ? "primary" : "ghost"}
            className="min-w-7 px-1.5 py-1 text-xs tabular-nums"
            aria-current={p === page ? "page" : undefined}
            onClick={() => onPage(p)}
          >
            {p + 1}
          </Button>
        ),
      )}
      <Button
        variant="secondary"
        className="px-2 py-1 text-xs"
        aria-label="Next page"
        title="Next page"
        disabled={page >= pageCount - 1}
        onClick={() => onPage(page + 1)}
      >
        →
      </Button>
    </nav>
  );
}
