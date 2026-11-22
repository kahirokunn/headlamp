/*
 * Copyright 2025 The Kubernetes Authors
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

import { IDBFactory, IDBObjectStore, IDBRequest } from 'fake-indexeddb';

it('reuses the same private import key after reload and retries failed reads', async () => {
  vi.stubGlobal('indexedDB', new IDBFactory());
  try {
    vi.resetModules();
    const first = await import('./importRevisionKey');
    const key = await first.getImportRevisionKey();
    expect(key).toMatch(/^[0-9a-f]{64}$/);
    vi.resetModules();
    const reloaded = await import('./importRevisionKey');
    vi.spyOn(indexedDB, 'open').mockImplementationOnce(() => {
      throw new Error('Unavailable');
    });
    await expect(reloaded.getImportRevisionKey()).rejects.toThrow('Unavailable');
    vi.spyOn(IDBObjectStore.prototype, 'get').mockImplementationOnce(() => {
      const read = new IDBRequest();
      Object.defineProperty(read, 'error', { value: new Error('Read failed') });
      queueMicrotask(() => read.onerror?.call(read, new Event('error')));
      return read;
    });
    await expect(reloaded.getImportRevisionKey()).rejects.toThrow('Read failed');
    expect(await reloaded.getImportRevisionKey()).toBe(key);
  } finally {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  }
});
