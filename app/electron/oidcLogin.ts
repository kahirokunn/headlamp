/*
 * Copyright 2026 The Kubernetes Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import { BrowserWindow } from 'electron';
import { isOIDCProviderNavigation } from './urlSafety';

/** Completes a desktop login in the originating session without loading provider content in the app. */
export async function openOIDCLogin(
  parent: BrowserWindow,
  backendOrigin: string,
  provider: URL,
  signal: AbortSignal
): Promise<boolean> {
  signal.throwIfAborted();
  const state = provider.searchParams.get('state');
  const callback = new URL(provider.searchParams.get('redirect_uri') ?? '');
  const backend = new URL(backendOrigin);
  const localCallback = ['localhost', '127.0.0.1', '[::1]'].includes(callback.hostname);
  if (
    !state ||
    !isOIDCProviderNavigation(provider, backendOrigin) ||
    !localCallback ||
    callback.protocol !== backend.protocol ||
    callback.port !== backend.port ||
    callback.pathname !== '/oidc-callback' ||
    callback.username ||
    callback.password
  ) {
    throw new Error('Invalid desktop OIDC redirect');
  }
  const session = parent.webContents.session;
  const popup = new BrowserWindow({
    parent,
    width: 800,
    height: 600,
    webPreferences: { session, nodeIntegration: false, contextIsolation: true, sandbox: true },
  });
  popup.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
  const closed = new AbortController();
  const lifetime = AbortSignal.any([signal, closed.signal, AbortSignal.timeout(5 * 60 * 1000)]);
  let completing = false;
  try {
    return await new Promise<boolean>((resolve, reject) => {
      lifetime.addEventListener('abort', () => resolve(false), { once: true });
      popup.once('closed', () => closed.abort());
      function navigate(event: Electron.Event<{ url: string; isMainFrame: boolean }>) {
        const target = new URL(event.url);
        if (!isOIDCProviderNavigation(target, backendOrigin)) {
          event.preventDefault();
          reject(new Error('Invalid OIDC navigation'));
          return;
        }
        if (target.origin !== callback.origin && target.origin !== backendOrigin) return;
        event.preventDefault();
        if (
          !event.isMainFrame ||
          target.origin !== callback.origin ||
          target.pathname !== callback.pathname ||
          target.searchParams.get('state') !== state ||
          !target.searchParams.get('code')
        ) {
          reject(new Error('Unexpected desktop OIDC callback'));
          return;
        }
        if (completing) return;
        completing = true;
        void session
          .fetch(new URL(target.pathname + target.search, backend).href, {
            headers: { Accept: 'application/json' },
            redirect: 'error',
            credentials: 'include',
            signal: lifetime,
          })
          .then(response => {
            if (response.status !== 204)
              throw new Error(`OIDC login failed (HTTP ${response.status})`);
            resolve(true);
          })
          .catch(reject);
      }
      popup.webContents.on('will-navigate', navigate);
      popup.webContents.on('will-redirect', navigate);
      void popup.loadURL(provider.href).catch(error => {
        // Intercepting an immediate callback cancels the provider navigation.
        if (!completing && !lifetime.aborted) reject(error);
      });
    });
  } finally {
    if (!popup.isDestroyed()) popup.destroy();
    closed.abort();
  }
}
