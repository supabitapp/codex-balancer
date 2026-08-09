import eslint from "@eslint/js";
import tseslint from "typescript-eslint";

const typescriptFiles = ["src/**/*.ts", "test/**/*.ts", "*.ts"];

export default tseslint.config(
  {
    ignores: [
      ".wrangler",
      "coverage",
      "dist",
      "eslint.config.mjs",
      "node_modules",
      "worker-configuration.d.ts",
    ],
  },
  eslint.configs.recommended,
  ...tseslint.configs.strictTypeChecked.map((config) => ({
    ...config,
    files: typescriptFiles,
  })),
  ...tseslint.configs.stylisticTypeChecked.map((config) => ({
    ...config,
    files: typescriptFiles,
  })),
  {
    files: typescriptFiles,
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
  },
  {
    files: ["public/**/*.js"],
    languageOptions: {
      globals: {
        document: "readonly",
        Element: "readonly",
        fetch: "readonly",
        HTMLButtonElement: "readonly",
        Intl: "readonly",
        navigator: "readonly",
        URL: "readonly",
        WebSocket: "readonly",
        window: "readonly",
      },
    },
  },
);
