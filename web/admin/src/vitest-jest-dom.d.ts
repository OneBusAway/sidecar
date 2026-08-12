// Brings jest-dom's element matchers (toBeInTheDocument, toHaveTextContent,
// toBeChecked, …) into scope for svelte-check.
//
// The runtime side is `vitest-setup-client.ts`, which the `component` vitest
// project loads. That import registers the matchers but is not part of the
// program svelte-check builds, so without this the matchers exist at run time
// and are type errors at check time -- a split that would push anyone writing
// a component test toward `as any`.
//
// The `/vitest` entry point, not the bare package: the default one declares
// the matchers on Jest's globals and drags in `@types/jest`, which is not
// installed and never will be.
import '@testing-library/jest-dom/vitest';
