import { useCallback, useEffect, useState } from 'react';
import { api, errorMessage } from '../lib/api';
import type { AuthStatus } from '../types';

export function useSession() {
  const [session, setSession] = useState<AuthStatus | null>(null);
  const [error, setError] = useState('');
  const expire = useCallback(
    () =>
      setSession((previous) => ({
        initialized: previous?.initialized ?? true,
        authenticated: false,
      })),
    [],
  );
  const refresh = useCallback(async () => {
    try {
      const status = await api<AuthStatus>('/api/auth/status');
      setSession(status);
      setError('');
      return status;
    } catch (error) {
      setError(errorMessage(error));
      throw error;
    }
  }, []);
  useEffect(() => {
    void refresh().catch(() => {});
  }, [refresh]);
  useEffect(() => {
    window.addEventListener('session-expired', expire);
    return () => window.removeEventListener('session-expired', expire);
  }, [expire]);
  useEffect(() => {
    if (!session?.authenticated || !session.expires_at) return;
    let timer: ReturnType<typeof setTimeout>;
    const arm = () => {
      const remaining = Date.parse(session.expires_at!) - Date.now();
      if (remaining <= 0 || !Number.isFinite(remaining)) {
        expire();
        return;
      }
      timer = setTimeout(arm, Math.min(remaining, 2147483647));
    };
    arm();
    const visible = () => {
      if (document.visibilityState === 'visible') void refresh().catch(() => {});
    };
    document.addEventListener('visibilitychange', visible);
    return () => {
      clearTimeout(timer);
      document.removeEventListener('visibilitychange', visible);
    };
  }, [session?.authenticated, session?.expires_at, expire, refresh]);
  const logout = useCallback(async () => {
    await api('/api/auth/logout', { method: 'POST' });
    expire();
  }, [expire]);
  return { session, setSession, error, refresh, expire, logout };
}
