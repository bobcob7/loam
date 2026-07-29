import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

// React Testing Library auto-registers this when `test.globals` is on, but
// registering it explicitly keeps the teardown true regardless of that flag.
afterEach(() => {
  cleanup();
});
