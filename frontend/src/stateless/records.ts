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

import type { Cluster } from '../lib/k8s/cluster';
import { storedContextNames, withoutKubeconfigContexts } from './storedContexts';

/**
 * DatabaseEvent is the event that is fired when the IndexedDB is upgraded.
 * It is used to create the object store. It is also used to get the result of the
 * IndexedDB open request. The result is the IndexedDB database. It is used to create
 * the transaction and the object store. It is also used to get the result of the
 * IndexedDB add request. The result is the key of the added object.
 * @see storeStatelessClusterKubeconfig
 * @see getStatelessClusterKubeConfigs
 * @see findKubeconfigByClusterName
 */
export interface DatabaseEvent extends Event {
  // target is the request that generated the event.
  target: IDBOpenDBRequest & {
    // result is the IndexedDB database. It is used to create the transaction and the object store.
    result: IDBDatabase;
  };
}

/** handleDatabaseUpgrade creates the object store if it doesn't exist.
 * this upgrade is only called when the database is created.
 * It is not called when the database is opened.
 * @param event - The event that is fired when the IndexedDB is upgraded.
 * @see storeStatelessClusterKubeconfig
 * @see getStatelessClusterKubeConfigs
 * @see findKubeconfigByClusterName
 * **/
export function handleDatabaseUpgrade(event: DatabaseEvent) {
  const db = event.target ? event.target.result : null;
  // Create the object store if it doesn't exist
  if (db && !db.objectStoreNames.contains('kubeconfigStore')) {
    db.createObjectStore('kubeconfigStore', { keyPath: 'id', autoIncrement: true });
  }
}

/**
 * Generates a cryptographically secure random token using the browser's crypto API.
 * @param {number} length - The length of the token.
 * @returns {string} - The generated token.
 */
export function generateSecureToken(length = 16): string {
  const buffer = new Uint8Array(length);
  if (import.meta.env.NODE_ENV === 'test') {
    // Use Math.random() in the testing environment
    return Array.from(buffer, () => Math.floor(Math.random() * 16).toString(16)).join('');
  }
  window.crypto.getRandomValues(buffer);
  return Array.from(buffer, byte => byte.toString(16).padStart(2, '0'))
    .join('')
    .slice(0, length);
}

/** New imports must have one unambiguous connection per name. */
export function assertUnambiguousImport(config?: {
  ambiguousContexts?: Array<{ name: string; kubeconfigIndex: number }>;
}) {
  const conflict = config?.ambiguousContexts?.find(item => item.kubeconfigIndex === 0);
  if (conflict)
    throw new Error(`Multiple kubeconfig contexts have the Headlamp name "${conflict.name}"`);
}

export interface StoredKubeconfig {
  id: number;
  kubeconfig: string;
  incarnations?: Record<string, string>;
  pending?: boolean;
}

export function importedClusterFromRecord(
  cluster: Cluster,
  recordId?: number,
  incarnation?: string
): Cluster {
  if (recordId === undefined) return cluster;
  const fingerprint = cluster.importRevision;
  const revision = /^import-[0-9a-f]{32}$/.test(fingerprint ?? '')
    ? [fingerprint, recordId, incarnation].filter(part => part !== undefined).join('-')
    : fingerprint;
  return {
    ...cluster,
    kubeconfigRecordId: recordId,
    ...(revision === undefined ? {} : { importRevision: revision }),
  };
}

export function sameImportedSelection(current: Cluster | undefined, expected: Cluster | undefined) {
  return (
    current === expected ||
    (current?.kubeconfigRecordId !== undefined &&
      current.kubeconfigRecordId === expected?.kubeconfigRecordId &&
      current.importRevision === expected?.importRevision)
  );
}

function changeRecord(change: (store: IDBObjectStore) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open('kubeconfigs', 1) as IDBOpenDBRequest;
    request.onupgradeneeded = event => handleDatabaseUpgrade(event as unknown as DatabaseEvent);
    request.onerror = () => reject(request.error);
    request.onsuccess = () => {
      const db = request.result;
      const transaction = db.transaction('kubeconfigStore', 'readwrite');
      transaction.oncomplete = () => {
        db.close();
        resolve();
      };
      transaction.onabort = () => {
        db.close();
        reject(transaction.error ?? new Error('Kubeconfig storage transaction was aborted'));
      };
      change(transaction.objectStore('kubeconfigStore'));
    };
  });
}

export function discardPendingKubeconfigRecord(id: number) {
  return changeRecord(store => {
    const request = store.get(id);
    request.onsuccess = () => {
      if ((request.result as StoredKubeconfig | undefined)?.pending) store.delete(id);
    };
  });
}

export interface DisplacedContext {
  name: string;
  kubeconfigRecordId?: number;
  incarnation?: string;
}

export function displacedImport(cluster: Cluster | undefined): DisplacedContext | undefined {
  if (!cluster) return undefined;
  const incarnation = cluster.importRevision?.match(
    /^import-[0-9a-f]{32}-[0-9]+-([0-9a-f]+)$/
  )?.[1];
  return { name: cluster.name, kubeconfigRecordId: cluster.kubeconfigRecordId, incarnation };
}

export function publishKubeconfigRecord(
  id: number,
  previous: Array<DisplacedContext | undefined> = [],
  validateSelection?: () => void,
  names?: string[]
) {
  let installationError: unknown;
  return changeRecord(store => {
    const request = store.get(id);
    request.onsuccess = () => {
      const record = request.result as StoredKubeconfig | undefined;
      if (!record) {
        store.transaction.abort();
        return;
      }
      // Validate the selection while write transactions are serialized.
      try {
        validateSelection?.();
      } catch (error) {
        installationError = error;
        store.transaction.abort();
        return;
      }
      const publish = () => {
        store.put({ ...record, pending: false });
        // Commit the selection and remove the displaced contexts in one transaction.
        // Group siblings so each record is read and rewritten once.
        const byRecord = new Map<number, DisplacedContext[]>();
        for (const cluster of previous) {
          const key = cluster?.kubeconfigRecordId;
          if (!cluster || key === undefined || key === id) continue;
          const contexts = byRecord.get(key) ?? [];
          contexts.push(cluster);
          byRecord.set(key, contexts);
        }
        for (const [key, contexts] of byRecord) {
          const get = store.get(key);
          get.onsuccess = () => {
            const old = get.result as StoredKubeconfig | undefined;
            if (!old) return;
            try {
              if (
                contexts.some(context => context.incarnation !== old.incarnations?.[context.name])
              ) {
                throw new Error('The stored kubeconfig changed during import');
              }
              const remaining = withoutKubeconfigContexts(
                old.kubeconfig,
                contexts.map(context => context.name)
              );
              if (remaining === null) store.delete(key);
              else if (remaining !== old.kubeconfig) store.put({ ...old, kubeconfig: remaining });
            } catch (error) {
              installationError = error;
              store.transaction.abort();
            }
          };
        }
      };
      if (names) {
        // Compare with the committed selections of every tab in this write transaction.
        const expected = new Map(
          names.map(name => [name, previous.find(c => c?.name === name)?.kubeconfigRecordId])
        );
        const cursor = store.openCursor(null, 'prev');
        cursor.onsuccess = () => {
          const row = cursor.result;
          const prior = row?.value as StoredKubeconfig | undefined;
          if (prior && !prior.pending && prior.id !== id) {
            try {
              for (const name of storedContextNames(prior.kubeconfig)) {
                if (!expected.has(name)) continue;
                if (expected.get(name) !== prior.id) {
                  installationError = new Error('The stored kubeconfig changed during import');
                  store.transaction.abort();
                  return;
                }
                expected.delete(name);
              }
            } catch {
              // Unreadable legacy rows do not provide a selectable context.
            }
          }
          if (row && expected.size) {
            row.continue();
            return;
          }
          if ([...expected.values()].some(key => key !== undefined)) {
            installationError = new Error('The selected kubeconfig is no longer stored');
            store.transaction.abort();
            return;
          }
          publish();
        };
      } else {
        publish();
      }
    };
  }).catch(error => {
    throw installationError ?? error;
  });
}

export async function renewRetiredImport(cluster: Cluster | undefined) {
  const recordId = cluster?.kubeconfigRecordId;
  const revision = cluster?.importRevision;
  const fingerprint = revision?.match(/^import-[0-9a-f]{32}/)?.[0];
  if (!cluster || recordId === undefined || !fingerprint) return;

  let renewedRevision: string | undefined;
  await changeRecord(store => {
    const request = store.get(recordId);
    request.onsuccess = () => {
      const record = request.result as StoredKubeconfig | undefined;
      if (!record) return;
      const incarnation = record.incarnations?.[cluster.name];
      const previous = importedClusterFromRecord(
        { ...cluster, importRevision: fingerprint },
        recordId,
        incarnation
      );
      if (previous.importRevision !== revision) {
        renewedRevision = previous.importRevision;
        return;
      }

      const generation = generateSecureToken();
      renewedRevision = [fingerprint, recordId, generation].join('-');
      store.put({
        ...record,
        incarnations: { ...record.incarnations, [cluster.name]: generation },
      });
    };
  });
  return renewedRevision;
}
