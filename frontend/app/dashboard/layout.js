import DashboardShell from "@/components/DashboardShell";

export const metadata = {
  title: { default: "Dashboard", template: "%s · Dashboard" },
  // Nothing behind a sign-in should be in an index.
  robots: { index: false, follow: false },
};

export default function DashboardLayout({ children }) {
  return <DashboardShell>{children}</DashboardShell>;
}
