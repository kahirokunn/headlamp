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

import * as jsyaml from 'js-yaml';
import { decodeBase64 } from '../helpers/base64';
import { KubeconfigObject } from '../lib/k8s/kubeconfig';
import store from '../redux/stores/store';
import {
  CursorSuccessEvent,
  DatabaseErrorEvent,
  DatabaseEvent,
  findMatchingContexts,
  handleDataBaseError,
  handleDatabaseUpgrade,
} from '.';
import type { StoredKubeconfig } from './records';

/**
 * Finds a kubeconfig by cluster name.
 * @param clusterName The name of the cluster to find.
 * @param clusterID The ID for a cluster, composed of the kubeconfig path and cluster name
 * @param recordId The selected import’s local storage record.
 * @returns A promise that resolves with the kubeconfig, or null if not found.
 * @throws Error if IndexedDB is not supported.
 * @throws Error if the kubeconfig is invalid.
 */

export function findKubeconfigByClusterName(
  /** The name of the cluster to find */
  clusterName: string,
  /** The ID for a cluster, composed of the kubeconfig path and cluster name */
  clusterID?: string,
  recordId = store.getState().config.statelessClusters?.[clusterName]?.kubeconfigRecordId,
  onFound?: (record: StoredKubeconfig) => void
): Promise<string | null> {
  return new Promise<string | null>((resolve, reject) => {
    const request = indexedDB.open('kubeconfigs', 1) as any;
    request.onupgradeneeded = handleDatabaseUpgrade;
    request.onerror = (event: DatabaseErrorEvent) => handleDataBaseError(event, reject);

    request.onsuccess = (event: DatabaseEvent) => {
      const db = event.target.result;

      try {
        const transaction = db.transaction(['kubeconfigStore'], 'readonly');
        transaction.oncomplete = () => db.close();
        transaction.onabort = () => {
          db.close();
          reject(transaction.error ?? new Error('Kubeconfig lookup was aborted'));
        };

        const cursorRequest = transaction
          .objectStore('kubeconfigStore')
          .openCursor(recordId, 'prev');
        cursorRequest.onerror = () => reject(cursorRequest.error);
        cursorRequest.onsuccess = (event: Event) => {
          const cursor = (event as CursorSuccessEvent).target.result;
          if (!cursor) {
            resolve(null);
            return;
          }

          try {
            if (cursor.value.pending) {
              cursor.continue();
              return;
            }
            const kubeconfig = cursor.value.kubeconfig;
            const parsedKubeconfig = jsyaml.load(decodeBase64(kubeconfig)) as KubeconfigObject;
            const { matchingKubeconfig, matchingContext } = findMatchingContexts(
              clusterName,
              parsedKubeconfig,
              clusterID
            );
            if (matchingKubeconfig || matchingContext) {
              onFound?.({ ...cursor.value, id: cursor.primaryKey as number });
              resolve(kubeconfig);
            } else {
              cursor.continue();
            }
          } catch (error) {
            reject(error);
          }
        };
      } catch (error) {
        db.close();
        reject(error);
      }
    };
  });
}
