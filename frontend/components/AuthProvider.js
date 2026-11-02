"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";

import { ApiError, request } from "@/lib/api";

/**
 * Holds the signed-in creator's session.
 *
 * The access token lives in memory and nowhere else — not localStorage, not a
 * readable cookie. Anything a script on this page can read, an injected script
 * can steal, and a stolen access token is a valid session for fifteen minutes
 * with no way to revoke it. The refresh token is the opposite: an HttpOnly
 * cookie scoped to /api/v1/auth, which JavaScript cannot read at all and the
 * browser only ever attaches to the endpoints that rotate it.
 *
 * The cost of memory-only is that a page reload loses the token. That is what
 * the refresh call on mount is for: the cookie survives, so the session is
 * rebuilt from it before anything renders.
 */
const AuthContext = createContext(null);

export function useAuth() {
  const context = useContext(AuthContext);
  if (!context) {
    throw new Error("useAuth must be used inside <AuthProvider>");
  }
  return context;
}

export function AuthProvider({ children }) {
  const [token, setToken] = useState(null);
  const [user, setUser] = useState(null);
  // "loading" until the refresh attempt settles, so protected pages can wait
  // rather than flashing a sign-in screen at someone who is already signed in.
  const [status, setStatus] = useState("loading");

  const refreshTimer = useRef(null);
  // applySession schedules the next refresh, and refresh calls applySession —
  // a genuine cycle. Holding the current refresh in a ref breaks it without a
  // forward reference that would capture a stale closure.
  const refreshRef = useRef(null);

  const applySession = useCallback((session) => {
    setToken(session.access_token);
    setUser(session.user);
    setStatus("authenticated");

    // Refresh a minute before expiry. Waiting for a 401 would mean one request
    // fails first, and the one that fails is whichever the creator happened to
    // make — including a form submission they then have to redo.
    clearTimeout(refreshTimer.current);
    const aheadMs = Math.max((session.expires_in - 60) * 1000, 30_000);
    refreshTimer.current = setTimeout(() => {
      refreshRef.current?.().catch(() => {});
    }, aheadMs);
  }, []);

  const refresh = useCallback(async () => {
    // Authenticated by the cookie, not by a token — which is the whole point:
    // it still works after the access token has expired.
    const session = await request("/api/v1/auth/refresh", { method: "POST" });
    applySession(session);
    return session;
  }, [applySession]);

  // Kept current in an effect rather than assigned during render, which would
  // be a side effect in a render pass.
  useEffect(() => {
    refreshRef.current = refresh;
  }, [refresh]);

  useEffect(() => {
    let active = true;

    // Rebuild the session from the refresh cookie on load. A failure here is
    // the normal case for a signed-out visitor, not an error.
    const timer = setTimeout(() => {
      refresh()
        .catch(() => {
          if (active) setStatus("anonymous");
        });
    }, 0);

    return () => {
      active = false;
      clearTimeout(timer);
      clearTimeout(refreshTimer.current);
    };
  }, [refresh]);

  const login = useCallback(
    async (email, password) => {
      const session = await request("/api/v1/auth/login", {
        method: "POST",
        body: { email, password },
      });
      applySession(session);
      return session;
    },
    [applySession],
  );

  const register = useCallback(
    async (email, password) => {
      const session = await request("/api/v1/auth/register", {
        method: "POST",
        body: { email, password },
      });
      applySession(session);
      return session;
    },
    [applySession],
  );

  const logout = useCallback(async () => {
    clearTimeout(refreshTimer.current);
    try {
      // Revokes the refresh token server-side. Clearing only the local state
      // would leave a working cookie behind, so "sign out" on a shared machine
      // would not actually sign anyone out.
      await request("/api/v1/auth/logout", { method: "POST" });
    } catch {
      // Already expired or revoked — the local session still goes.
    }
    setToken(null);
    setUser(null);
    setStatus("anonymous");
  }, []);

  /**
   * Makes an authenticated request, refreshing once if the token has expired.
   *
   * The retry exists because a token can expire between the timer firing and a
   * request landing — a laptop that was asleep, a slow network. Retrying once
   * on 401 turns that from a visible failure into nothing the creator notices.
   */
  const authedRequest = useCallback(
    async (path, options = {}) => {
      try {
        return await request(path, { ...options, token });
      } catch (error) {
        if (!(error instanceof ApiError) || !error.isUnauthorized) {
          throw error;
        }
        const session = await refresh();
        return request(path, { ...options, token: session.access_token });
      }
    },
    [token, refresh],
  );

  const value = useMemo(
    () => ({ token, user, status, login, register, logout, refresh, authedRequest }),
    [token, user, status, login, register, logout, refresh, authedRequest],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
