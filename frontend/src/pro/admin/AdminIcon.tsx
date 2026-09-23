// Small, shared outline icons keep the admin navigation visually consistent.
export function AdminIcon({ name }: { name: string }) {
  const paths: Record<string, string> = {
    overview: 'M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z',
    users: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M16 3a4 4 0 0 1 0 8M22 21v-2a4 4 0 0 0-3-3.87M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0',
    'signup-risk': 'M12 3l8 3v6c0 5-8 9-8 9s-8-4-8-9V6zM8 12l3 3 5-6',
    promotions: 'M10 13a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-2 2M14 11a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l2-2',
    plans: 'M3 5h18v14H3zM3 10h18M7 15h3',
    models: 'M12 3l9 5-9 5-9-5zM3 12l9 5 9-5M3 16l9 5 9-5',
    tenants: 'M4 21V7h8v14M12 3h8v18M2 21h20M7 11h2M7 15h2M15 7h2M15 11h2M15 15h2',
    settings: 'M4 7h16M4 17h16M8 4v6M16 14v6',
    back: 'M14 5l-7 7 7 7M7 12h13',
  }
  return <svg className="pa-icon" viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={paths[name] || paths.overview} /></svg>
}
