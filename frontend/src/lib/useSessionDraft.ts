// useSessionDraft: a draft value mirrored into sessionStorage so it survives the
// editor being UNMOUNTED. AssessmentDetail renders each tab conditionally
// ({tab === 'masking' && <MaskingTab/>}), so switching tabs tears the editor down
// and drops all of its useState — losing minutes of precise rectangle drawing with
// no prompt (HCI audit finding C). Persisting the in-progress draft keyed by
// assessment + editor kind lets the next mount restore it.
//
// `draft` is null until the user first edits, so the caller falls back to the server
// value; this also stays correct when the server data arrives AFTER mount (the ID
// editor mounts before its query resolves). Call clear() on a successful Save after
// putting the saved value in the query cache: it drops both copies so later server
// refetches remain visible instead of being permanently masked by a stale draft.

import { useCallback, useState } from "react";

export interface SessionDraft<T> {
  /** null = untouched since mount/save; caller should fall back to the server value. */
  draft: T | null;
  /** Update the working value and mirror it into sessionStorage. */
  setDraft: (next: T) => void;
  /** Drop both the persisted and in-memory copies and lower `dirty`. */
  clear: () => void;
  /** True while an unsaved, persisted draft exists — drives the "will be restored" hint. */
  dirty: boolean;
}

function readDraft<T>(key: string): T | null {
  try {
    const raw = sessionStorage.getItem(key);
    return raw === null ? null : (JSON.parse(raw) as T);
  } catch {
    // Malformed JSON or sessionStorage unavailable — treat as no draft.
    return null;
  }
}

export function useSessionDraft<T>(key: string): SessionDraft<T> {
  type DraftState = { key: string; draft: T | null; dirty: boolean };
  const restore = (storageKey: string): DraftState => {
    const restored = readDraft<T>(storageKey);
    return { key: storageKey, draft: restored, dirty: restored !== null };
  };

  // AssessmentDetail can retain the same editor component while its route id
  // changes. Key the in-memory value too, otherwise assessment A's draft can be
  // displayed — and saved — after navigating directly to assessment B.
  const [state, setState] = useState<DraftState>(() => restore(key));
  let current = state;
  if (state.key !== key) {
    current = restore(key);
    setState(current);
  }

  const setDraft = useCallback(
    (next: T) => {
      setState({ key, draft: next, dirty: true });
      try {
        sessionStorage.setItem(key, JSON.stringify(next));
      } catch {
        // Storage full/unavailable: the draft simply won't survive an unmount.
      }
    },
    [key],
  );

  const clear = useCallback(() => {
    setState({ key, draft: null, dirty: false });
    try {
      sessionStorage.removeItem(key);
    } catch {
      // ignore
    }
  }, [key]);

  return { draft: current.draft, setDraft, clear, dirty: current.dirty };
}
