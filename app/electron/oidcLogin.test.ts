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
import { EventEmitter } from 'node:events';
import { beforeEach, expect, it, vi } from 'vitest';
import { openOIDCLogin } from './oidcLogin';

vi.mock('electron', () => ({ BrowserWindow: vi.fn() }));
const backend = 'http://127.0.0.1:4466';
const callback = backend + '/oidc-callback';
const provider = new URL('https://login.example/authorize');
provider.search = new URLSearchParams({ state: 'attempt-a', redirect_uri: callback }).toString();
const exchange = vi.fn();
const session = { fetch: exchange };
const parent = { webContents: { session } } as unknown as BrowserWindow;
const contents = Object.assign(new EventEmitter(), { setWindowOpenHandler: vi.fn() });
const popup = Object.assign(new EventEmitter(), {
  webContents: contents,
  isDestroyed: () => false,
  destroy: vi.fn(),
  loadURL: vi.fn().mockResolvedValue(undefined),
});
beforeEach(() => {
  vi.clearAllMocks();
  contents.removeAllListeners();
  popup.removeAllListeners();
  vi.mocked(BrowserWindow).mockImplementation(function () {
    return popup as unknown as BrowserWindow;
  });
});
function returnFromProvider(state: string, callbackUrl = callback) {
  const event = {
    url: callbackUrl + '?state=' + state + '&code=accepted',
    isMainFrame: true,
    preventDefault: vi.fn(),
  };
  contents.emit('will-redirect', event);
  expect(event.preventDefault).toHaveBeenCalledOnce();
}
it('completes only after the backend commits the matching callback to the desktop session', async () => {
  const pending = Promise.withResolvers<Response>();
  exchange.mockReturnValueOnce(pending.promise);
  const result = openOIDCLogin(parent, backend, provider, new AbortController().signal);
  expect(BrowserWindow).toHaveBeenCalledWith(
    expect.objectContaining({
      webPreferences: { session, nodeIntegration: false, contextIsolation: true, sandbox: true },
    })
  );
  returnFromProvider('attempt-a');
  expect(popup.destroy).not.toHaveBeenCalled();
  pending.resolve(new Response(null, { status: 204 }));
  await expect(result).resolves.toBe(true);
  expect(popup.destroy).toHaveBeenCalledOnce();
});
it('closes a superseded login while its callback is pending', async () => {
  const controller = new AbortController();
  exchange.mockReturnValueOnce(new Promise(() => {}));
  const result = openOIDCLogin(parent, backend, provider, controller.signal);
  returnFromProvider('attempt-a');
  controller.abort();
  await expect(result).resolves.toBe(false);
  expect(exchange.mock.calls[0][1].signal.aborted).toBe(true);
  expect(popup.destroy).toHaveBeenCalledOnce();
});
it('rejects a callback from a different attempt before exchanging its code', async () => {
  const result = openOIDCLogin(parent, backend, provider, new AbortController().signal);
  returnFromProvider('attempt-b');
  await expect(result).rejects.toThrow('Unexpected desktop OIDC callback');
  expect(exchange).not.toHaveBeenCalled();
});

it('exchanges a localhost callback on the backend origin used by the app', async () => {
  const alias = 'http://localhost:4466/oidc-callback';
  const login = new URL(provider);
  login.searchParams.set('redirect_uri', alias);
  exchange.mockResolvedValueOnce(new Response(null, { status: 204 }));
  const result = openOIDCLogin(parent, backend, login, new AbortController().signal);
  returnFromProvider('attempt-a', alias);
  await expect(result).resolves.toBe(true);
  expect(exchange).toHaveBeenCalledWith(
    callback + '?state=attempt-a&code=accepted',
    expect.anything()
  );
});
