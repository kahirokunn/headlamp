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

import store from '../redux/stores/store';
import {
  CursorSuccessEvent,
  DatabaseErrorEvent,
  DatabaseEvent,
  handleDataBaseError,
  handleDatabaseUpgrade,
} from '.';
import { withoutKubeconfigContexts } from './storedContexts';

/**
 * deleteClusterKubeconfig deletes the kubeconfig for a stateless cluster from indexedDB
 * @param clusterName - The name of the cluster
 * @param clusterID The ID for a cluster, composed of the kubeconfig path and cluster name
 * @returns A promise that resolves with the kubeconfig, or null if not found.
 */
export async function deleteClusterKubeconfig(
  clusterName: string,
  clusterID?: string,
  recordId = store.getState().config.statelessClusters?.[clusterName]?.kubeconfigRecordId
): Promise<string | null> {
  return new Promise<string | null>((resolve, reject) => {
    try {
      const request = indexedDB.open('kubeconfigs', 1) as any;

      // The onupgradeneeded event is fired when the database is created for the first time.
      request.onupgradeneeded = handleDatabaseUpgrade;

      // The onsuccess event is fired when the database is opened.
      // This event is where you specify the actions to take when the database is opened.
      request.onsuccess = function handleDatabaseSuccess(event: DatabaseEvent) {
        const db = event.target.result;
        const transaction = db.transaction(['kubeconfigStore'], 'readwrite');
        const store = transaction.objectStore('kubeconfigStore');
        let deleted: string | null = null;
        transaction.oncomplete = () => {
          db.close();
          resolve(deleted);
        };
        transaction.onabort = () => {
          db.close();
          reject(transaction.error ?? new Error('Kubeconfig deletion was aborted'));
        };

        // The onsuccess event is fired when the request has succeeded.
        // This is where you handle the results of the request.
        // The result is the cursor. It is used to iterate through the object store.
        // The cursor is null when there are no more objects to iterate through.
        // The cursor is used to find the kubeconfig by cluster name.
        store.openCursor(recordId, 'prev').onsuccess = function storeSuccess(event: Event) {
          // delete the kubeconfig by cluster name
          const successEvent = event as CursorSuccessEvent;
          const cursor = successEvent.target.result;

          // when we do not find a cursor, we resolve with null
          if (!cursor) {
            return;
          }

          const row = cursor.value;
          if (row.pending) {
            cursor.continue();
            return;
          }
          const remaining = withoutKubeconfigContexts(row.kubeconfig, [clusterName], clusterID);
          if (remaining === row.kubeconfig) {
            cursor.continue();
            return;
          }
          deleted = row.kubeconfig;
          if (remaining === null) store.delete(cursor.key);
          else store.put({ ...row, kubeconfig: remaining });
        };
      };

      // The onerror event is fired when the database is opened.
      // This is where you handle errors.
      request.onerror = (event: DatabaseErrorEvent) => handleDataBaseError(event, reject);
    } catch (error) {
      reject(error);
    }
  });
}
