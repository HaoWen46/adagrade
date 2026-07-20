import { Link, useLocation, useNavigate, useSearchParams } from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import { internalPathTarget, internalRedirectTarget } from "../lib/auth";
import { Button, Card } from "../components/ui";

// Landing page for emailed sign-in links. The link is a plain GET here —
// nothing is consumed by fetching it, so mail-scanner prefetch can't burn the
// one-time token. Only the explicit button click POSTs the token.
export function EmailCallback() {
  const [searchParams] = useSearchParams();
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const token = searchParams.get("token") ?? "";

  // A magic link normally opens in a fresh tab with no router history state. The
  // server therefore carries the validated return path in the emailed URL. Same-tab
  // history state wins when present; both inputs are open-redirect guarded.
  const stateRedirect = internalRedirectTarget(location.state);
  const redirectTo =
    stateRedirect !== "/" ? stateRedirect : internalPathTarget(searchParams.get("return_to"));

  const complete = useMutation({
    mutationFn: () => api.post<unknown>("/auth/email-callback", { token }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      navigate(redirectTo, { replace: true });
    },
  });

  return (
    <div className="flex min-h-screen items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <div className="pt-2 text-center">
          <h1 className="text-xl font-semibold tracking-tight text-neutral-900">AdaGrade</h1>
          <p className="mt-1 text-sm text-neutral-500">
            AI-assisted grading — Algorithm Design &amp; Analysis
          </p>
        </div>

        {!token ? (
          <div className="mt-6 space-y-3 text-center">
            <p className="text-sm text-neutral-500">
              This sign-in link is incomplete. Request a new one from the login page.
            </p>
            <Link to="/login" className="text-sm text-neutral-900 underline">
              Back to login
            </Link>
          </div>
        ) : (
          <div className="mt-6 space-y-3">
            <Button
              type="button"
              variant="primary"
              className="w-full"
              disabled={complete.isPending}
              onClick={() => complete.mutate()}
            >
              {complete.isPending ? "Signing in…" : "Complete sign-in"}
            </Button>
            {complete.isError && (
              <div className="space-y-1 text-center">
                <p className="text-xs text-red-600">{complete.error.message}</p>
                <Link to="/login" className="text-sm text-neutral-900 underline">
                  Request a new link
                </Link>
              </div>
            )}
          </div>
        )}
      </Card>
    </div>
  );
}
