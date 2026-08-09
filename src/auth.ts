import { secureEqual } from "./crypto.js";

const bearerAuthorization = /^Bearer ([A-Za-z0-9._~+/-]+={0,})$/u;
const bearerToken = /^[A-Za-z0-9._~+/-]+={0,}$/u;

export interface AuthenticationFailure {
  readonly message: string;
  readonly ok: false;
  readonly status: 401 | 500;
}

export type BearerAuthResult = AuthenticationFailure | { readonly ok: true };

export const parseBearerAuthorization = (
  value: string | null,
): string | undefined => value?.match(bearerAuthorization)?.[1];

export const authenticateBearer = async (
  request: Request,
  expected: string,
): Promise<BearerAuthResult> => {
  if (!bearerToken.test(expected)) {
    return {
      message: "server bearer key is not configured",
      ok: false,
      status: 500,
    };
  }

  const presented = parseBearerAuthorization(
    request.headers.get("Authorization"),
  );
  if (presented === undefined || !(await secureEqual(presented, expected))) {
    return {
      message: "missing or invalid bearer key",
      ok: false,
      status: 401,
    };
  }

  return { ok: true };
};
