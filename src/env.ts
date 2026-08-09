import type { BalancerState } from "./state.js";

export interface Env {
  ACCESS_AUD: string;
  ACCESS_TEAM_DOMAIN: string;
  ASSETS: Fetcher;
  AUTH_BASE_URL: string;
  BALANCER_KEY: string;
  BALANCER_STATE: DurableObjectNamespace<BalancerState>;
  GIT_SHA: string;
  TOKEN_ENCRYPTION_KEY: string;
  UPSTREAM_BASE_URL: string;
  USAGE_BASE_URL: string;
}

export const stateStub = (env: Env): DurableObjectStub<BalancerState> =>
  env.BALANCER_STATE.getByName("global");
