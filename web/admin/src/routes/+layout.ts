// Pure client-rendered SPA: no SSR (there is no Node server in production)
// and no prerendering (it conflicts with the index.html fallback).
export const ssr = false;
export const prerender = false;
