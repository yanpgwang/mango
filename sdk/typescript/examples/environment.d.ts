// This SDK intentionally has no Node runtime dependency. Only the example
// reads process.env; the browser-compatible client itself does not.
declare const process: { env: Record<string, string | undefined> };
