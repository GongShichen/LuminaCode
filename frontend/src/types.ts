export type RpcError = { code: string; message: string };

export type RpcResponse = {
  id: string;
  ok: boolean;
  result?: unknown;
  error?: RpcError;
};

export type PushEvent = {
  type: "event";
  session_id?: string;
  seq?: number;
  event: {
    type: string;
    payload: unknown;
  };
};

export type TranscriptEntry = {
  kind: "user" | "assistant";
  text: string;
};

export type LaunchOptions = {
  cwd: string;
  resumeSessionID?: string;
};
