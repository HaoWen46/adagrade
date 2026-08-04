import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import { internalRedirectTarget, useMe } from "../lib/auth";
import { Button, Card, Input, Spinner, buttonClassName } from "../components/ui";

interface AuthModes {
  email: boolean;
  google: boolean;
  dev: boolean;
}

export function Login() {
  const me = useMe();
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [email, setEmail] = useState("");
  const [sentTo, setSentTo] = useState("");

  // Where a 401 bounce wanted to land (RequireAuth stashes it in history state).
  // Falls back to "/" when absent; guarded against open-redirect. Used by both the
  // already-authenticated early return and the dev-login success, so whichever path
  // fires sends the TA to the same place instead of the roster root.
  const redirectTo = internalRedirectTarget(location.state);

  const modes = useQuery({
    queryKey: ["auth-modes"],
    queryFn: () => api.get<AuthModes>("/api/auth/modes"),
    staleTime: Infinity,
    retry: 1,
  });

  const emailLogin = useMutation({
    mutationFn: (loginEmail: string) =>
      api.post<unknown>("/auth/email-login", { email: loginEmail, return_to: redirectTo }),
    onSuccess: (_data, loginEmail) => {
      setSentTo(loginEmail);
    },
  });

  const devLogin = useMutation({
    mutationFn: (loginEmail: string) => api.post<unknown>("/auth/dev-login", { email: loginEmail }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      navigate(redirectTo, { replace: true });
    },
  });

  // react-query keeps stale `me` data after a 401-driven refetch fails; without
  // the error check this would bounce /login → / → /login forever.
  if (me.data?.user && !me.error) {
    return <Navigate to={redirectTo} replace />;
  }

  const submitDevLogin = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = email.trim();
    if (trimmed) devLogin.mutate(trimmed);
  };

  const submitEmailLogin = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = email.trim();
    if (trimmed) emailLogin.mutate(trimmed);
  };

  return (
    <div className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <div className="pt-2 text-center">
          <h1 className="text-xl font-semibold tracking-tight text-neutral-900">AdaGrade</h1>
          <p className="mt-1 text-sm text-neutral-500">
            AI-assisted grading — Algorithm Design &amp; Analysis
          </p>
        </div>

        {modes.isPending && (
          <div className="mt-6 flex justify-center">
            <Spinner className="size-6" />
          </div>
        )}

        {modes.isError && (
          <div className="mt-6 space-y-3 text-center">
            <p className="text-sm text-red-600">
              Couldn&apos;t load sign-in options — the server may be starting up or
              unreachable.
            </p>
            <Button
              variant="secondary"
              disabled={modes.isFetching}
              onClick={() => void modes.refetch()}
            >
              {modes.isFetching ? "Retrying…" : "Retry"}
            </Button>
          </div>
        )}

        {modes.data?.email && (
          <form className="mt-6 space-y-3" onSubmit={submitEmailLogin}>
            <Input
              type="email"
              required
              autoComplete="email"
              placeholder="you@ntu.edu.tw"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
            <Button
              type="submit"
              variant="primary"
              className="w-full"
              disabled={emailLogin.isPending}
            >
              {emailLogin.isPending ? "Sending link…" : "Send sign-in link"}
            </Button>
            {sentTo && (
              <>
                <p className="text-sm text-neutral-500">
                  Check your email for a sign-in link.
                </p>
                <p className="text-xs text-neutral-400">
                  No email after a few minutes? Ask an admin to confirm your address is on the
                  allowlist, then request a new link.
                </p>
              </>
            )}
            {emailLogin.isError && (
              <p className="text-xs text-red-600">{emailLogin.error.message}</p>
            )}
          </form>
        )}

        {modes.data?.google && (
          <div className={modes.data?.email ? "mt-4 border-t border-neutral-200 pt-4" : "mt-6"}>
            <a
              href={redirectTo === "/" ? "/auth/login" : `/auth/login?return_to=${encodeURIComponent(redirectTo)}`}
              className={buttonClassName("secondary", "w-full")}
            >
              Sign in with Google
            </a>
          </div>
        )}

        {modes.data?.dev && (
          <form
            className="mt-6 space-y-2 border-t border-neutral-200 pt-4"
            onSubmit={submitDevLogin}
          >
            <p className="text-xs font-medium tracking-wide text-neutral-400 uppercase">
              Dev login
            </p>
            <p className="text-xs text-neutral-500">
              Use the bootstrap admin email configured in <code>.env</code>.
            </p>
            <Input type="email" required autoComplete="email" placeholder="you@example.com" value={email} onChange={(e) => setEmail(e.target.value)} />
            <Button
              type="submit"
              variant="secondary"
              className="w-full"
              disabled={devLogin.isPending}
            >
              {devLogin.isPending ? "Signing in…" : "Sign in (dev)"}
            </Button>
            {devLogin.isError && (
              <p className="text-xs text-red-600">{devLogin.error.message}</p>
            )}
          </form>
        )}
      </Card>
    </div>
  );
}
