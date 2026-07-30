// Tiny typed fetch wrapper for the ADA-Marker API.
//
// - JSON in/out, same-origin credentials (session cookie).
// - Every mutating call sends `X-ADA-CSRF: 1` (docs/DECISIONS.md D7: the custom
//   header forces a CORS preflight cross-origin; same-origin SPA fetch supplies
//   it trivially).
// - 401 responses throw UnauthorizedError so callers (auth guard) can redirect.

export class UnauthorizedError extends Error {
  constructor(message = "unauthorized") {
    super(message);
    this.name = "UnauthorizedError";
  }
}

export class ApiError extends Error {
  readonly status: number;
  /** Parsed JSON error body when the server sent one (e.g. roster line errors). */
  readonly details?: unknown;

  constructor(status: number, message: string, details?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.details = details;
  }
}

async function handle<T>(res: Response): Promise<T> {
  if (res.status === 401) {
    // Nudge the auth provider (auth.tsx listens) to re-check the session so an
    // expired login redirects to /login instead of stranding a stale page.
    window.dispatchEvent(new Event("ada:unauthorized"));
    throw new UnauthorizedError();
  }
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`;
    let details: unknown;
    try {
      const body = await res.text();
      if (body) {
        try {
          const parsed: unknown = JSON.parse(body);
          if (
            parsed !== null &&
            typeof parsed === "object" &&
            "error" in parsed &&
            typeof (parsed as { error: unknown }).error === "string"
          ) {
            message = (parsed as { error: string }).error;
            details = parsed;
          } else {
            message = body;
          }
        } catch {
          message = body;
        }
      }
    } catch {
      // keep the status-line message
    }
    throw new ApiError(res.status, message, details);
  }
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (method !== "GET") {
    headers["X-ADA-CSRF"] = "1";
  }
  const init: RequestInit = { method, credentials: "same-origin", headers };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  return handle<T>(await fetch(path, init));
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body?: unknown) => request<T>("POST", path, body),
  put: <T>(path: string, body?: unknown) => request<T>("PUT", path, body),
  patch: <T>(path: string, body?: unknown) => request<T>("PATCH", path, body),
  del: <T>(path: string, body?: unknown) => request<T>("DELETE", path, body),
};

/** Multipart upload (roster CSV, submission PDFs). Browser sets the boundary. */
export async function apiUpload<T>(path: string, form: FormData): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: { "X-ADA-CSRF": "1" },
    body: form,
  });
  return handle<T>(res);
}

/**
 * Reads the download filename out of a Content-Disposition header, preferring the
 * RFC 5987 `filename*=UTF-8''…` form (it carries the encoding) over the plain
 * `filename="…"` form. Returns null when the header is absent or carries neither.
 */
function filenameFromDisposition(header: string | null): string | null {
  if (!header) return null;
  const extended = /filename\*=\s*UTF-8''([^;]+)/i.exec(header);
  if (extended) {
    try {
      return decodeURIComponent(extended[1].trim());
    } catch {
      // Malformed percent-encoding — fall through to the plain form.
    }
  }
  const plain = /filename\s*=\s*"([^"]*)"|filename\s*=\s*([^;]+)/i.exec(header);
  return plain?.[1] ?? plain?.[2]?.trim() ?? null;
}

/** The server picks the download name, so treat it as untrusted: no path separators,
 * no control characters, no leading dots. Empty after cleaning falls back. */
function safeFilename(name: string, fallback: string): string {
  const cleaned = name
    .replace(/[/\\]/g, "_")
    .replace(/[\u0000-\u001f\u007f]/g, "")
    .replace(/^\.+/, "")
    .trim();
  return cleaned === "" ? fallback : cleaned;
}

/**
 * Binary download (transcription-export ZIP). Fetches with the session cookie, then
 * hands the bytes to the browser through an object URL.
 *
 * Deliberately not a plain `<a href download>`: the transcription ZIP can take
 * 20–60s to build on its first (uncached) request, so the caller needs a real
 * in-flight state and a real error — a bare anchor gives neither, and a server error
 * would silently render as a broken file. Throws ApiError/UnauthorizedError like the
 * rest of this client, so callers can drive it with useMutation.
 */
export async function apiDownload(path: string, fallbackFilename: string): Promise<void> {
  const res = await fetch(path, { method: "GET", credentials: "same-origin" });
  if (!res.ok) {
    // Always throws (UnauthorizedError on 401, ApiError otherwise).
    await handle<unknown>(res);
    return;
  }
  const blob = await res.blob();
  const name = safeFilename(
    filenameFromDisposition(res.headers.get("Content-Disposition")) ?? fallbackFilename,
    fallbackFilename,
  );
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.rel = "noopener";
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoking synchronously after click() cancels the download in some browsers —
  // release the object URL on the next macrotask instead.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

/**
 * Task T2: RegradeDetail.snapshot arrives as a base64 string — Go's encoding/json
 * marshals a `[]byte` struct field that way, not as embedded raw JSON — so decode then
 * parse it here rather than duplicating this gotcha at every call site. Returns null on
 * any decode/parse failure (absent snapshot, malformed payload) so callers can render a
 * fallback instead of throwing.
 */
export function decodeRegradeSnapshot<T>(base64: string | undefined): T | null {
  if (!base64) return null;
  try {
    // atob yields Latin-1 code units; must decode as UTF-8 bytes to preserve multi-byte sequences (e.g. Chinese names).
    const json = JSON.parse(new TextDecoder().decode(Uint8Array.from(atob(base64), c => c.charCodeAt(0)))) as T;
    return json;
  } catch {
    return null;
  }
}
