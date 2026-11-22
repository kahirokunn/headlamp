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

export const IMPORT_REVISION_KEY_HEADER = 'X-Headlamp-Import-Key';

let revisionKey: Promise<string> | undefined;

/** Database creation atomically chooses one private key for all tabs sharing these imports. */
export function getImportRevisionKey(): Promise<string> {
  revisionKey ??= new Promise<string>((resolve, reject) => {
    const request = indexedDB.open('headlamp-import-revision-key', 1);
    request.onupgradeneeded = () => {
      const bytes = crypto.getRandomValues(new Uint8Array(32));
      const key = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
      request.result.createObjectStore('keys').add(key, 'import');
    };
    request.onerror = () => reject(request.error);
    request.onsuccess = () => {
      const db = request.result;
      const read = db.transaction('keys').objectStore('keys').get('import');
      db.close();
      read.onerror = () => reject(read.error);
      read.onsuccess = () => resolve(read.result);
    };
  }).catch(error => {
    revisionKey = undefined;
    throw error;
  });
  return revisionKey;
}
