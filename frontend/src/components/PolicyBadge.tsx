// Shared visual language for grading policy (D25): lenient=green, standard=indigo,
// strict=amber. regrade_strict (D50/D53 AI re-grade assist) reuses the strict tone —
// it's the same "exam standard" stance, just pinned to the regrade-assist flow instead
// of a curated grading method. One place so the tone mapping can't drift between pages.

import type { BadgeTone } from "./ui";
import { Badge } from "./ui";

export const POLICY_TONES: Record<string, BadgeTone> = {
  lenient: "green",
  standard: "indigo",
  strict: "amber",
  regrade_strict: "amber",
};

// Human-readable labels for policy values that don't read well verbatim
// (e.g. "regrade_strict" as raw text). Values absent here render as the raw
// policy string, matching the pre-existing behavior for lenient/standard/strict.
const POLICY_LABELS: Record<string, string> = {
  regrade_strict: "AI re-grade (strict)",
};

export function policyTone(policy: string): BadgeTone {
  return POLICY_TONES[policy] ?? "neutral";
}

export function policyLabel(policy: string): string {
  return POLICY_LABELS[policy] ?? policy;
}

/** Small badge for a policy key, e.g. in record history or analysis rows. */
export function PolicyBadge({ policy, className }: { policy: string; className?: string }) {
  return (
    <Badge tone={policyTone(policy)} className={className}>
      {policyLabel(policy)}
    </Badge>
  );
}
