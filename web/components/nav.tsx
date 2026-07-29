"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { cn } from "@/lib/utils";

const NAV_ITEMS = [
  { href: "/tasks", label: "Task" },
  { href: "/agents", label: "Agent" },
  { href: "/runtimes", label: "Runtime" },
];

export function Nav() {
  const pathname = usePathname();
  return (
    <nav className="w-52 shrink-0 border-r border-zinc-200 bg-white flex flex-col gap-0.5 p-3">
      <div className="px-3 py-3 text-sm font-bold text-zinc-900 tracking-tight">
        agentwork
      </div>
      {NAV_ITEMS.map((item) => {
        const active = pathname.startsWith(item.href);
        return (
          <Link
            key={item.href}
            href={item.href}
            className={cn(
              "px-3 py-2 rounded-md text-sm font-medium transition-colors",
              active
                ? "bg-zinc-900 text-white"
                : "text-zinc-600 hover:bg-zinc-100 hover:text-zinc-900"
            )}
          >
            {item.label}
          </Link>
        );
      })}
    </nav>
  );
}
