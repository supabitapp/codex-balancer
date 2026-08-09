import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    exclude: ["test/**/*.worker.test.ts"],
    include: ["test/**/*.test.ts"],
  },
});
