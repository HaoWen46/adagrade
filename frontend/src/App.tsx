import { Navigate, NavLink, Outlet, Route, Routes } from "react-router";
import { api } from "./lib/api";
import { RequireAuth, roleAtLeast, useMe, type Role } from "./lib/auth";
import { Badge, Button, cx } from "./components/ui";
import { Login } from "./pages/Login";
import { EmailCallback } from "./pages/EmailCallback";
import { Assessments } from "./pages/Assessments";
import { AssessmentDetail } from "./pages/AssessmentDetail";
import { ProblemReview } from "./pages/ProblemReview";
import { AnswerView } from "./pages/AnswerView";
import { Students } from "./pages/Students";
import { StudentPage } from "./pages/StudentPage";
import { Users } from "./pages/Users";
import { Methods } from "./pages/Methods";
import { Providers } from "./pages/Providers";
import { Runs } from "./pages/Runs";
import { Regrades } from "./pages/Regrades";
import { GuidePage } from "./pages/GuidePage";

// Order follows setup flow: providers must exist before methods reference them.
const NAV_ITEMS: Array<{ to: string; label: string; minRole?: Role }> = [
  { to: "/assessments", label: "Assessments" },
  { to: "/students", label: "Students" },
  { to: "/providers", label: "Providers" },
  { to: "/methods", label: "Methods" },
  { to: "/runs", label: "Runs" },
  { to: "/regrades", label: "Regrade inbox", minRole: "ta" },
  { to: "/guide", label: "Guide" },
  { to: "/users", label: "Users", minRole: "admin" },
];

function Shell() {
  const me = useMe();
  const user = me.data?.user;

  const signOut = async () => {
    try {
      await api.post("/auth/logout");
    } finally {
      window.location.reload();
    }
  };

  return (
    <div className="flex min-h-screen">
      <aside className="flex w-52 shrink-0 flex-col bg-neutral-900 text-neutral-300">
        <div className="px-4 py-4">
          <span className="text-sm font-semibold tracking-tight text-white">AdaGrade</span>
        </div>
        <nav className="flex flex-1 flex-col gap-0.5 px-2">
          {NAV_ITEMS.filter(
            (item) => !item.minRole || roleAtLeast(user?.role, item.minRole),
          ).map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                cx(
                  "rounded-md px-2.5 py-1.5 text-sm transition-colors",
                  isActive
                    ? "bg-neutral-800 font-medium text-white"
                    : "hover:bg-neutral-800/60 hover:text-white",
                )
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
        <div className="px-4 py-3 text-[11px] text-neutral-500">AI-assisted grading</div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-12 shrink-0 items-center justify-end gap-3 border-b border-neutral-200 bg-white px-4">
          {user && (
            <>
              <span className="text-sm text-neutral-600">{user.email}</span>
              <Badge tone="indigo">{user.role}</Badge>
            </>
          )}
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs"
            onClick={() => void signOut()}
          >
            Sign out
          </Button>
        </header>
        <main className="flex-1 p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/login/email" element={<EmailCallback />} />
      <Route
        element={
          <RequireAuth>
            <Shell />
          </RequireAuth>
        }
      >
        <Route index element={<Navigate to="/assessments" replace />} />
        <Route path="assessments" element={<Assessments />} />
        <Route path="assessments/:id" element={<AssessmentDetail />} />
        <Route path="assessments/:aid/problems/:pid/review" element={<ProblemReview />} />
        <Route path="answers/:id" element={<AnswerView />} />
        <Route path="students" element={<Students />} />
        {/* :sid is the school ID (students.student_id), not the DB id — same vocabulary
            as the totals table, the CSV, and the export filenames. */}
        <Route path="students/:sid" element={<StudentPage />} />
        <Route path="methods" element={<Methods />} />
        <Route path="providers" element={<Providers />} />
        <Route path="runs" element={<Runs />} />
        <Route path="regrades" element={<Regrades />} />
        <Route path="guide" element={<GuidePage />} />
        <Route path="users" element={<Users />} />
        <Route path="*" element={<Navigate to="/assessments" replace />} />
      </Route>
    </Routes>
  );
}
