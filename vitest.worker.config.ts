import { cloudflareTest } from "@cloudflare/vitest-pool-workers";
import { defineConfig } from "vitest/config";

const secrets = {
  BALANCER_KEY: "test-balancer-key",
  TOKEN_ENCRYPTION_KEY: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
};

Object.assign(process.env, secrets);

export default defineConfig({
  plugins: [
    cloudflareTest({
      miniflare: {
        bindings: {
          ACCESS_AUD: "test-audience",
          ACCESS_TEAM_DOMAIN: "team.cloudflareaccess.com",
          AUTH_BASE_URL: "https://auth.test",
          GIT_SHA: "test-sha",
          UPSTREAM_BASE_URL: "https://upstream.test",
          USAGE_BASE_URL: "https://usage.test",
          ...secrets,
        },
        compatibilityDate: "2026-08-08",
      },
      wrangler: {
        configPath: "./wrangler.jsonc",
      },
    }),
  ],
  test: {
    include: ["test/**/*.worker.test.ts"],
  },
});
