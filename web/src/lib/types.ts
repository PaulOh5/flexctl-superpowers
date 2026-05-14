export type User = { id: string; slug: string; email: string }
export type Env = {
  id: string; node_id: string; template_id: string; name: string;
  hostname: string; status: 'creating' | 'running' | 'stopped' | 'error';
  status_message?: string;
  sidecar_container_id?: string; dev_container_id?: string;
  gpu_request: number; gpu_indices: number[] | null;
}
export type Node = {
  id: string; name: string; status: string;
  agent_version?: string; gpu_info?: unknown;
  last_seen_at?: string;
}
export type ImageTemplate = {
  id: string; display_name?: string; description?: string;
  image_ref: string; default_cmd: string[] | null;
}
export type SSHKey = {
  id: string; name: string; public_key: string; fingerprint: string;
}
export type Device = {
  id: string; name: string; hostname: string;
  created_at: string; last_seen_at?: string;
}
