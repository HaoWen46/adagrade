// Auth context: one shared useQuery of GET /api/me + a route guard.

import { createContext, useContext, useEffect, type ReactNode } from "react";
import { useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { Navigate, useLocation } from "react-router";
import { api, UnauthorizedError } from "./api";
import { Button, Card, Spinner } from "../components/ui";

export interface User {
  id: number;
  email: string;
  display_name: string;
  role: string;
}

export interface MeResponse {
  user: User;
}

// Role ladder (plan §8): admin > lecturer > ta. Mirrors requireRole server-side —
// the UI only hides controls; the server is the enforcement point.

export type Role = "ta" | "lecturer" | "admin";

const ROLE_RANK: Record<string, number> = { ta: 1, lecturer: 2, admin: 3 };

/** True when `role` sits at or above `min` on the ladder (unknown roles rank 0). */
export function roleAtLeast(role: string | undefined, min: Role): boolean {
  return (ROLE_RANK[role ?? ""] ?? 0) >= ROLE_RANK[min];
}

/** Return a same-origin, root-relative path or the safe root fallback. */
export function internalPathTarget(value: unknown): string {
  if (
    typeof value !== "string" ||
    !value.startsWith("/") ||
    value.startsWith("//") ||
    value.includes("\\") ||
    /[\r\n\0]/.test(value)
  ) {
    return "/";
  }
  return value;
}

/**
 * Safe post-login redirect target derived from a react-router history-state `from`
 * (the Location that RequireAuth stashes when a 401 bounces the user to /login).
 * Returns an internal path (pathname + search + hash), or "/" when `from` is absent
 * or unusable. Open-redirect guard: only a `from` whose pathname is root-relative is
 * honored — absolute URLs and protocol-relative ("//host") paths are ignored, so a
 * crafted history state can't send a re-login off to another origin.
 */
export function internalRedirectTarget(state: unknown): string {
  const from = (state as { from?: unknown } | null | undefined)?.from;
  if (from && typeof from === "object") {
    const loc = from as { pathname?: unknown; search?: unknown; hash?: unknown };
    if (
      typeof loc.pathname === "string" &&
      loc.pathname.startsWith("/") &&
      !loc.pathname.startsWith("//")
    ) {
      const search = typeof loc.search === "string" ? loc.search : "";
      const hash = typeof loc.hash === "string" ? loc.hash : "";
      return internalPathTarget(loc.pathname + search + hash);
    }
  }
  return "/";
}

type MeQuery = UseQueryResult<MeResponse, Error>;

const AuthContext = createContext<MeQuery | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const query = useQuery<MeResponse, Error>({
    queryKey: ["me"],
    queryFn: () => api.get<MeResponse>("/api/me"),
    staleTime: 60_000,
    retry: (failureCount, error) =>
      !(error instanceof UnauthorizedError) && failureCount < 2,
  });

  // Any API 401 (api.ts dispatches "ada:unauthorized") re-checks the session so
  // RequireAuth redirects to /login instead of stranding an expired page. The
  // /api/me re-check itself 401s and fires this event again, so guard against a
  // dispatch→refetch loop: skip when signed out (no data yet), while the
  // re-check is already in flight, and once the 401 has been recorded. Cache
  // state is read fresh via the queryClient — the render-scoped `query` would
  // be stale inside the handler.
  useEffect(() => {
    const onUnauthorized = () => {
      const state = queryClient.getQueryState<MeResponse>(["me"]);
      if (
        state?.data !== undefined &&
        state.fetchStatus !== "fetching" &&
        !(state.error instanceof UnauthorizedError)
      ) {
        void queryClient.refetchQueries({ queryKey: ["me"] });
      }
    };
    window.addEventListener("ada:unauthorized", onUnauthorized);
    return () => window.removeEventListener("ada:unauthorized", onUnauthorized);
  }, [queryClient]);

  return <AuthContext.Provider value={query}>{children}</AuthContext.Provider>;
}

export function useMe(): MeQuery {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useMe must be used within <AuthProvider>");
  return ctx;
}

/** Renders children only for an authenticated user; 401 redirects to /login. */
export function RequireAuth({ children }: { children: ReactNode }) {
  const me = useMe();
  const location = useLocation();

  if (me.isPending) {
    return (
      <div className="flex h-screen items-center justify-center">
        <Spinner className="size-6" />
      </div>
    );
  }
  if (me.error instanceof UnauthorizedError) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }
  if (me.error) {
    return (
      <div className="flex h-screen items-center justify-center p-4">
        <Card title="Cannot reach the server" className="w-full max-w-sm">
          <p className="text-sm text-neutral-500">{me.error.message}</p>
          <Button variant="secondary" className="mt-3" onClick={() => void me.refetch()}>
            Retry
          </Button>
        </Card>
      </div>
    );
  }
  return <>{children}</>;
}
