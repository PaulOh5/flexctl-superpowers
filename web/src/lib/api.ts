import type { User, Env, Node, ImageTemplate, SSHKey, Device } from './types'

export class ApiError extends Error {
  status: number
  constructor(message: string, status: number) { super(message); this.status = status }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: 'include',
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  if (res.status === 401) {
    throw new ApiError('unauthorized', 401)
  }
  if (!res.ok) {
    let msg = res.statusText
    try { const j = await res.json(); if (j?.error) msg = j.error } catch {}
    throw new ApiError(msg, res.status)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

export const api = {
  me:           () => request<User>('GET', '/v1/me'),
  signup:       (b: { email: string; slug: string; password: string }) => request<void>('POST', '/v1/auth/signup', b),
  login:        (b: { email: string; password: string }) => request<void>('POST', '/v1/auth/login', b),
  logout:       () => request<void>('POST', '/v1/auth/logout'),

  listEnvs:     () => request<Env[]>('GET', '/v1/envs'),
  getEnv:       (id: string) => request<Env>('GET', `/v1/envs/${id}`),
  createEnv:    (b: { name: string; template_id: string; node_id: string; gpu_request: number }) =>
                  request<Env>('POST', '/v1/envs', b),
  stopEnv:      (id: string) => request<void>('POST', `/v1/envs/${id}/stop`),
  startEnv:     (id: string) => request<void>('POST', `/v1/envs/${id}/start`),
  deleteEnv:    (id: string) => request<void>('DELETE', `/v1/envs/${id}`),

  listNodes:    () => request<Node[]>('GET', '/v1/nodes'),
  listTemplates:() => request<ImageTemplate[]>('GET', '/v1/image-templates'),

  listKeys:     () => request<SSHKey[]>('GET', '/v1/me/ssh-keys'),
  addKey:       (b: { name: string; public_key: string }) => request<SSHKey>('POST', '/v1/me/ssh-keys', b),
  deleteKey:    (id: string) => request<void>('DELETE', `/v1/me/ssh-keys/${id}`),

  listDevices:  () => request<Device[]>('GET', '/v1/devices'),
  deleteDevice: (id: string) => request<void>('DELETE', `/v1/devices/${id}`),
}
