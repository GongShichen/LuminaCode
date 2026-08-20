export type RpcError = { code: string; message: string };

export type RpcResponse = {
  id: string;
  ok: boolean;
  result?: unknown;
  error?: RpcError;
};

export type PushEvent = {
  type: "event";
  protocol_version?: number;
  session_id?: string;
  stream_id?: string;
  seq?: number;
  after_seq?: number;
  event_id?: string;
  event_type?: string;
  schema_version?: number;
  durable?: boolean;
  timestamp?: string;
  payload?: unknown;
  event?: {
    type: string;
    payload: unknown;
  };
};

export type RuntimeEvent = {
  seq: number;
  stream_seq?: number;
  event_id?: string;
  session_id?: string;
  stream_id?: string;
  type: string;
  schema_version?: number;
  occurred_at?: string;
  correlation_id?: string;
  audience?: string[];
  payload: any;
};

export type TranscriptEntry = {
  kind: "user" | "assistant";
  text: string;
};

export type LaunchOptions = {
  cwd: string;
  resumeSessionID?: string;
};
