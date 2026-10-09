export class APIError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message);
  }
}
export async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(path, {
    credentials: 'same-origin',
    cache: 'no-store',
    ...options,
    headers: { 'Content-Type': 'application/json', ...options.headers },
  });
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    if (response.status === 401 && !path.startsWith('/api/auth/') && path !== '/api/admin') {
      window.dispatchEvent(new Event('session-expired'));
    }
    throw new APIError(body.error || '请求失败（HTTP ' + response.status + '）', response.status);
  }
  return response.status === 204 ? (undefined as T) : response.json();
}
export const jsonBody = (value: unknown): RequestInit => ({ body: JSON.stringify(value) });
export const errorMessage = (error: unknown) =>
  error instanceof Error ? error.message : String(error);
