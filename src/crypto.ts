const encoder = new TextEncoder();
const decoder = new TextDecoder();

const encodeBase64Url = (bytes: Uint8Array): string => {
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary)
    .replaceAll("+", "-")
    .replaceAll("/", "_")
    .replace(/=+$/u, "");
};

const decodeBase64Url = (value: string): Uint8Array<ArrayBuffer> => {
  const normalized = value.replaceAll("-", "+").replaceAll("_", "/");
  const padding = "=".repeat((4 - (normalized.length % 4)) % 4);
  const binary = atob(normalized + padding);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
};

const importEncryptionKey = async (secret: string): Promise<CryptoKey> => {
  let bytes: Uint8Array<ArrayBuffer>;
  try {
    bytes = decodeBase64Url(secret);
  } catch {
    throw new Error("TOKEN_ENCRYPTION_KEY must contain 32 bytes");
  }
  if (bytes.byteLength !== 32) {
    throw new Error("TOKEN_ENCRYPTION_KEY must contain 32 bytes");
  }
  return crypto.subtle.importKey("raw", bytes, "AES-GCM", false, [
    "decrypt",
    "encrypt",
  ]);
};

export const encryptSecret = async (
  plaintext: string,
  secret: string,
  associatedData: string,
): Promise<string> => {
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const ciphertext = await crypto.subtle.encrypt(
    {
      additionalData: encoder.encode(associatedData),
      iv: nonce,
      name: "AES-GCM",
    },
    await importEncryptionKey(secret),
    encoder.encode(plaintext),
  );
  return `v1.${encodeBase64Url(nonce)}.${encodeBase64Url(new Uint8Array(ciphertext))}`;
};

export const decryptSecret = async (
  encoded: string,
  secret: string,
  associatedData: string,
): Promise<string> => {
  const [version, nonce, ciphertext, extra] = encoded.split(".");
  if (
    version !== "v1" ||
    nonce === undefined ||
    ciphertext === undefined ||
    extra !== undefined
  ) {
    throw new Error("invalid encrypted secret");
  }
  const plaintext = await crypto.subtle.decrypt(
    {
      additionalData: encoder.encode(associatedData),
      iv: decodeBase64Url(nonce),
      name: "AES-GCM",
    },
    await importEncryptionKey(secret),
    decodeBase64Url(ciphertext),
  );
  return decoder.decode(plaintext);
};

export const hashToken = async (token: string): Promise<string> =>
  encodeBase64Url(
    new Uint8Array(
      await crypto.subtle.digest("SHA-256", encoder.encode(token)),
    ),
  );

export const decodeJwtPayload = (token: string): unknown => {
  const parts = token.split(".");
  if (parts.length !== 3 || parts[1] === undefined) {
    return undefined;
  }
  try {
    return JSON.parse(decoder.decode(decodeBase64Url(parts[1]))) as unknown;
  } catch {
    return undefined;
  }
};

export const randomToken = (byteLength = 32): string => {
  if (!Number.isSafeInteger(byteLength) || byteLength < 16) {
    throw new Error("token length must be at least 16 bytes");
  }
  return encodeBase64Url(crypto.getRandomValues(new Uint8Array(byteLength)));
};

export const secureEqual = async (
  presented: string,
  expected: string,
): Promise<boolean> => {
  const [left, right] = await Promise.all([
    crypto.subtle.digest("SHA-256", encoder.encode(presented)),
    crypto.subtle.digest("SHA-256", encoder.encode(expected)),
  ]);
  const leftBytes = new Uint8Array(left);
  const rightBytes = new Uint8Array(right);
  let difference = 0;
  for (let index = 0; index < leftBytes.length; index += 1) {
    difference |= (leftBytes[index] ?? 0) ^ (rightBytes[index] ?? 0);
  }
  return difference === 0;
};
