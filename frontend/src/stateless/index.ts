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

import _ from 'lodash';
import { addBackstageAuthHeaders } from '../helpers/addBackstageAuthHeaders';
import { getHeadlampAPIHeaders } from '../helpers/getHeadlampAPIHeaders';
import { request } from '../lib/k8s/api/v1/clusterRequests';
import { JSON_HEADERS } from '../lib/k8s/api/v1/constants';
import { createDiscoveryConnectionChangedError } from '../lib/k8s/api/v2/discoveryConnection';
import { Cluster } from '../lib/k8s/cluster';
import { ConfigState, setStatelessConfig } from '../redux/configSlice';
import store from '../redux/stores/store';
import { deleteClusterKubeconfig } from './deleteClusterKubeconfig';
import { findKubeconfigByClusterName } from './findKubeconfigByClusterName';
import { getUserIdFromLocalStorage } from './getUserIdFromLocalStorage';
import { installKubeconfigRecord } from './installKubeconfigRecord';
import {
  assertUnambiguousImport,
  DatabaseEvent,
  displacedImport,
  handleDatabaseUpgrade,
  importedClusterFromRecord,
  sameImportedSelection,
  StoredKubeconfig,
} from './records';
import { retireImportedConnection } from './retireConnections';
import { rollbackKubeconfigImport } from './rollbackKubeconfigImport';
import { updateStatelessClusterKubeconfig } from './updateStatelessClusterKubeconfig';

export { findMatchingContexts } from './storedContexts';
export { generateSecureToken, handleDatabaseUpgrade } from './records';
export type { DatabaseEvent } from './records';

/**
 * ParsedConfig is the object that is fetched from the backend.
 * It contains a list of parsed cluster entries under the `clusters` field.
 * Each item corresponds to a parsed cluster object used by stateless config state.
 * @see fetchStatelessClusterKubeConfigs
 */
interface ParsedConfig {
  ambiguousContexts?: Array<{ name: string; kubeconfigIndex: number }>;
  clusters?: Cluster[];
}

/**
 * Converts parsed `/parseKubeConfig` response data into the redux stateless state shape.
 *
 * @param parsedConfig Parsed backend response that may include a `clusters` array.
 * @returns Object with `statelessClusters` indexed by cluster name.
 */
export function toStatelessConfigState(
  parsedConfig: ParsedConfig | null | undefined,
  recordIds?: number[],
  incarnations?: Array<Record<string, string> | undefined>
) {
  const statelessClusters: NonNullable<ConfigState['statelessClusters']> = {};

  if (parsedConfig?.clusters && Array.isArray(parsedConfig.clusters)) {
    parsedConfig.clusters.forEach(({ kubeconfigIndex, ...cluster }: Cluster) => {
      const index = kubeconfigIndex ?? (recordIds?.length === 1 ? 0 : undefined);
      const recordId = index === undefined ? undefined : recordIds?.[index];
      const incarnation = index === undefined ? undefined : incarnations?.[index]?.[cluster.name];
      statelessClusters[cluster.name] = importedClusterFromRecord(cluster, recordId, incarnation);
    });
  }

  return { statelessClusters };
}

/**
 * Merges freshly parsed stateless clusters into the currently loaded stateless map.
 *
 * Existing clusters are preserved, and clusters with matching names are overwritten
 * by the latest parsed values.
 *
 * @param currentStatelessClusters Current stateless cluster map from redux state.
 * @param parsedConfig Newly parsed backend response.
 * @returns Merged `statelessClusters` payload suitable for `setStatelessConfig`.
 */
export function mergeStatelessConfigState(
  currentStatelessClusters: ConfigState['statelessClusters'],
  parsedConfig: ParsedConfig | null | undefined,
  recordIds?: number[],
  incarnations?: Array<Record<string, string> | undefined>
) {
  const nextState = toStatelessConfigState(parsedConfig, recordIds, incarnations);

  return {
    statelessClusters: {
      ...(currentStatelessClusters || {}),
      ...nextState.statelessClusters,
    },
  };
}

/**
 * DatabaseErrorEvent is the event that is fired when the IndexedDB is upgraded.
 * It is used to get the error of the IndexedDB open request. It is also used to get
 * the error of the IndexedDB add request.
 * @see storeStatelessClusterKubeconfig
 * @see getStatelessClusterKubeConfigs
 * @see findKubeconfigByClusterName
 */
export interface DatabaseErrorEvent extends Event {
  // target is the request that generated the event.
  target: IDBOpenDBRequest & {
    // error is the error of the request.
    error: DOMException | null;
  };
}

/**
 * CursorSuccessEvent is the event that is fired when the IndexedDB is upgraded.
 * It is used to get the result of the IndexedDB open request. The result is the
 * cursor. It is used to iterate through the object store. It is also used to get
 * the result of the IndexedDB add request. The result is the cursor. It is used to
 * iterate through the object store.
 * @see getStatelessClusterKubeConfigs
 * @see findKubeconfigByClusterName
 * */
export interface CursorSuccessEvent extends Event {
  // target is the request that generated the event.
  target: EventTarget & {
    // result is the cursor. It is used to iterate through the object store.
    result: IDBCursorWithValue | null;
  };
}

/** handleDataBaseError handles errors when opening IndexedDB.
 * @param event - The event that is fired when the IndexedDB is upgraded.
 * @see storeStatelessClusterKubeconfig
 * @see getStatelessClusterKubeConfigs
 * @see findKubeconfigByClusterName
 * */

export function handleDataBaseError(event: DatabaseErrorEvent, reject: (reason?: any) => void) {
  console.error(event.target ? event.target.error : 'An error occurred while opening IndexedDB');
  reject(event.target ? event.target.error : 'An error occurred while opening IndexedDB');
}

/**
 * Store the kubeconfig for a stateless cluster in IndexedDB.
 * @param kubeconfig - The kubeconfig to store.
 * @param onStored - Receives the record key after the transaction commits.
 * @returns promise that resolves when the kubeconfig is successfully added.
 * @throws Error if IndexedDB is not supported.
 * @throws Error if the kubeconfig is invalid.
 */
export function storeStatelessClusterKubeconfig(
  kubeconfig: string,
  onStored?: (id: number) => void,
  pending = false
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const request = indexedDB.open('kubeconfigs', 1) as any;

    // The onupgradeneeded event is fired when the database is created for the first time.
    request.onupgradeneeded = handleDatabaseUpgrade;

    /** The onsuccess event is fired when the database is opened.
     * This event is where you specify the actions to take when the database is opened.
     * Once the database is opened, it creates a transaction and an object store.
     * The transaction is used to add the kubeconfig to the object store.
     * The object store is used to store the kubeconfig.
     * */
    request.onsuccess = function handleDatabaseSuccess(event: DatabaseEvent) {
      const db = event.target ? event.target.result : null;

      if (db) {
        const transaction = db.transaction(['kubeconfigStore'], 'readwrite');
        const store = transaction.objectStore('kubeconfigStore');

        const newItem = { kubeconfig, ...(pending ? { pending: true } : {}) };
        const addRequest = store.add(newItem);

        transaction.oncomplete = () => {
          db.close();
          try {
            onStored?.(addRequest.result as number);
            resolve();
          } catch (error) {
            reject(error);
          }
        };
        transaction.onabort = () => {
          db.close();
          reject(transaction.error ?? new Error('Kubeconfig could not be stored'));
        };
      } else {
        console.error('Failed to open IndexedDB');
        reject('Failed to open IndexedDB');
      }
    };

    // The onerror event is fired when the database is opened.
    // This is where you handle errors.
    request.onerror = (event: DatabaseErrorEvent) => handleDataBaseError(event, reject);
  });
}

/**
 * Gets stateless cluster kubeconfigs from IndexedDB.
 * @returns A promise that resolves with the kubeconfigs.
 * @throws Error if IndexedDB is not supported.
 * @throws Error if the kubeconfig is invalid.
 */
export async function getStatelessClusterKubeConfigs(): Promise<string[]> {
  return (await getStatelessKubeconfigRecords()).map(record => record.kubeconfig);
}

function getStatelessKubeconfigRecords(): Promise<StoredKubeconfig[]> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open('kubeconfigs', 1) as any;

    // The onupgradeneeded event is fired when the database is created for the first time.
    request.onupgradeneeded = handleDatabaseUpgrade;

    /** The onsuccess event is fired when the database is opened.
     * This event is where you specify the actions to take when the database is opened.
     * Once the database is opened, it creates a transaction and an object store.
     * It returns all the kubeconfigs from the object store.
     * */
    request.onsuccess = function handleDatabaseSuccess(event: DatabaseEvent) {
      const db = event.target ? event.target.result : null;

      if (db) {
        const transaction = db.transaction(['kubeconfigStore'], 'readonly');
        const store = transaction.objectStore('kubeconfigStore');

        const kubeconfigs: StoredKubeconfig[] = [];
        transaction.oncomplete = () => db.close();
        transaction.onabort = () => {
          db.close();
          reject(transaction.error ?? new Error('Kubeconfig lookup was aborted'));
        };

        /** The onsuccess event is fired when the request has succeeded.
         * This is where you handle the results of the request.
         * The result is the cursor. It is used to iterate through the object store.
         * The cursor is null when there are no more objects to iterate through.
         * */
        const cursorRequest = store.openCursor();
        cursorRequest.onerror = () => reject(cursorRequest.error);
        cursorRequest.onsuccess = function storeSuccess(event: Event) {
          const successEvent = event as CursorSuccessEvent;
          const cursor = successEvent.target.result;
          if (cursor) {
            if (cursor.value.pending) {
              cursor.continue();
              return;
            }
            kubeconfigs.push({
              id: cursor.primaryKey as number,
              kubeconfig: cursor.value.kubeconfig,
              incarnations: cursor.value.incarnations,
            });
            cursor.continue();
          } else {
            // All kubeconfigs have been retrieved
            resolve(kubeconfigs);
          }
        };
      } else {
        reject('Failed to open IndexedDB');
      }
    };

    // The onerror event is fired when the database is opened.
    // This is where you handle errors.
    request.onerror = (event: DatabaseErrorEvent) => handleDataBaseError(event, reject);
  });
}

/**
 * Finds and replaces a kubeconfig by cluster name.
 * @param clusterName - The name of the cluster to find and replace.
 * @param kubeconfig - The base64 encoded kubeconfig to replace the existing one with.
 * @param create - If true, create a new kubeconfig if it doesn't exist. If false, only replace existing kubeconfigs.
 * @returns A promise that resolves when the kubeconfig is successfully replaced.
 * @throws Error if the kubeconfig replacement fails at any step.
 *
 */
export async function findAndReplaceKubeconfig(
  clusterName: string,
  kubeconfig: string,
  create: boolean = false
): Promise<void> {
  const selected = store.getState().config.statelessClusters?.[clusterName];
  let previousRecord: StoredKubeconfig | undefined;
  const existingKubeconfig = await findKubeconfigByClusterName(
    clusterName,
    undefined,
    selected?.kubeconfigRecordId,
    record => {
      previousRecord = record;
    }
  );
  if (!existingKubeconfig && !create) {
    throw new Error(`No existing kubeconfig found for cluster ${clusterName} and create is false`);
  }

  const recoverPrevious = !selected?.importRevision && existingKubeconfig;
  const parsePrevious = recoverPrevious && existingKubeconfig !== kubeconfig;
  const inputs = parsePrevious ? [kubeconfig, existingKubeconfig] : [kubeconfig];
  const config = await parseStatelessKubeconfigs(inputs);
  assertUnambiguousImport(config);
  const replacementConfig = {
    clusters: config?.clusters?.filter(cluster => (cluster.kubeconfigIndex ?? 0) === 0),
  };
  const replacement = replacementConfig.clusters?.find(cluster => cluster.name === clusterName);
  if (!replacement) {
    throw new Error(`The kubeconfig does not contain cluster ${clusterName}`);
  }

  if (replacementConfig.clusters?.some(cluster => cluster.name !== clusterName)) {
    throw new Error(`The replacement kubeconfig must contain only cluster ${clusterName}`);
  }

  const assertStillSelected = () => {
    if (
      !sameImportedSelection(store.getState().config.statelessClusters?.[clusterName], selected)
    ) {
      throw createDiscoveryConnectionChangedError();
    }
  };
  const prior = recoverPrevious
    ? parsePrevious
      ? config?.clusters?.find(
          cluster => cluster.kubeconfigIndex === 1 && cluster.name === clusterName
        )
      : replacement
    : selected;
  const previous =
    prior &&
    importedClusterFromRecord(
      prior,
      previousRecord?.id,
      previousRecord?.incarnations?.[clusterName]
    );

  assertStillSelected();
  let recordId: number | undefined;
  await storeStatelessClusterKubeconfig(
    kubeconfig,
    id => {
      recordId = id;
    },
    true
  );

  const isInstalled = () =>
    recordId !== undefined &&
    store.getState().config.statelessClusters?.[clusterName]?.kubeconfigRecordId === recordId;
  let retirementStarted = false;
  try {
    if (!isInstalled()) {
      assertStillSelected();
      retirementStarted = true;
      await retireImportedConnection(previous);
      assertStillSelected();
      await installKubeconfigRecord(
        recordId,
        previousRecord
          ? [
              {
                name: clusterName,
                kubeconfigRecordId: previousRecord.id,
                incarnation: selected?.importRevision
                  ? displacedImport(selected)?.incarnation
                  : previousRecord.incarnations?.[clusterName],
              },
            ]
          : [],
        [clusterName],
        () => {
          assertStillSelected();
          return mergeStatelessConfigState(
            store.getState().config.statelessClusters,
            replacementConfig,
            recordId === undefined ? undefined : [recordId]
          );
        }
      );
    }
  } catch (error) {
    if (!isInstalled()) {
      await rollbackKubeconfigImport(recordId, retirementStarted ? [previous] : []);
      throw error;
    }
  }
}

/**
 * Compares the cluster config from the backend and the redux store
 * @param clusters
 * @param clustersToConfig
 * @returns true if the present stored config is different from the fetched one.
 */
export function isEqualClusterConfigs(
  currentConfig: ConfigState['clusters'],
  newConfig: ConfigState['clusters']
): boolean {
  if (!currentConfig || !newConfig) {
    return true; // Config is different if either is null/undefined
  }

  const currentKeys = Object.keys(currentConfig);
  const newKeys = Object.keys(newConfig);

  if (currentKeys.length !== newKeys.length) {
    return true; // Different number of clusters
  }

  return currentKeys.some(key => {
    if (!newConfig[key]) {
      return true; // Cluster in current config doesn't exist in new config
    }
    const currentCluster = _.omit(currentConfig[key], ['useToken']);
    const newCluster = _.omit(newConfig[key], ['useToken']);
    return !_.isEqual(currentCluster, newCluster);
  });
}

/**
 * Parses the cluster config from the backend and updates the redux store
 * if the present stored config is different from the fetched one.
 */
export async function fetchStatelessClusterKubeConfigs(dispatch: any): Promise<void> {
  const original = store.getState().config.statelessClusters;
  const records = await getStatelessKubeconfigRecords().catch((error: unknown) => {
    console.error('Error reading stored kubeconfigs:', error);
    return undefined;
  });
  if (!records) return;
  if (store.getState().config.statelessClusters !== original) return;

  const nextConfig =
    records.length > 0
      ? parseStatelessKubeconfigs(records.map(record => record.kubeconfig))
      : Promise.resolve({ clusters: [] });

  return nextConfig
    .then(async (config: ParsedConfig) => {
      const nextState = toStatelessConfigState(
        config,
        records.map(record => record.id),
        records.map(record => record.incarnations)
      );
      if (store.getState().config.statelessClusters !== original) return;
      if (!isEqualClusterConfigs(original, nextState.statelessClusters)) return;
      if (!original && Object.keys(nextState.statelessClusters).length === 0) return;

      const previous = original
        ? Object.values(original)
        : (config.clusters ?? []).map(cluster => {
            const record = records[cluster.kubeconfigIndex ?? 0];
            return importedClusterFromRecord(
              cluster,
              record?.id,
              record?.incarnations?.[cluster.name]
            );
          });
      const retirements = await Promise.allSettled(
        previous.map(cluster =>
          retireImportedConnection(
            cluster,
            nextState.statelessClusters[cluster.name]?.importRevision,
            true
          )
        )
      );
      const failed = new Set<string>();
      retirements.forEach((result, index) => {
        if (result.status === 'rejected') {
          failed.add(previous[index].name);
          console.error('Error retiring an imported connection:', result.reason);
        }
      });

      // Another tab may have published, removed or renewed a record during parsing
      // or retirement. Renewals of displaced contexts do not change the winner.
      const stored = await getStatelessKubeconfigRecords();
      if (
        stored.length !== records.length ||
        stored.some(
          (record, index) =>
            record.id !== records[index].id || record.kubeconfig !== records[index].kubeconfig
        )
      )
        return fetchStatelessClusterKubeConfigs(dispatch);
      const selectedRecords = new Map(stored.map((record, index) => [record.id, index]));
      for (const cluster of Object.values(nextState.statelessClusters)) {
        const index = selectedRecords.get(cluster.kubeconfigRecordId!);
        if (
          index !== undefined &&
          stored[index].incarnations?.[cluster.name] !== records[index].incarnations?.[cluster.name]
        )
          return fetchStatelessClusterKubeConfigs(dispatch);
      }

      const current = store.getState().config.statelessClusters;
      if (current === original && failed.size === 0) {
        dispatch(setStatelessConfig(nextState));
        return;
      }

      // A concurrent edit owns the entries it changed. Finish each independent
      // transition whose original selection is still present.
      const reconciled = { ...current };
      for (const name of new Set([
        ...Object.keys(original ?? {}),
        ...Object.keys(nextState.statelessClusters),
      ])) {
        if (failed.has(name) || !sameImportedSelection(current?.[name], original?.[name])) continue;
        const next = nextState.statelessClusters[name];
        if (next) reconciled[name] = next;
        else delete reconciled[name];
      }
      if (isEqualClusterConfigs(current, reconciled)) {
        dispatch(setStatelessConfig({ statelessClusters: reconciled }));
      }
    })
    .catch((err: Error) => {
      console.error('Error getting config:', err);
    });
}

function parseStatelessKubeconfigs(kubeconfigs: string[]): Promise<ParsedConfig> {
  const headers = addBackstageAuthHeaders(JSON_HEADERS);
  return request(
    '/parseKubeConfig',
    {
      method: 'POST',
      body: JSON.stringify({ kubeconfigs }),
      headers: { ...headers, ...getHeadlampAPIHeaders() },
    },
    false,
    false
  );
}

const exportFunctions = {
  storeStatelessClusterKubeconfig,
  getStatelessClusterKubeConfigs,
  findKubeconfigByClusterName,
  getUserIdFromLocalStorage,
  isEqualClusterConfigs,
  fetchStatelessClusterKubeConfigs,
  deleteClusterKubeconfig,
  updateStatelessClusterKubeconfig,
  // @deprecated - use isEqualClusterConfigs instead
  processClusterComparison: isEqualClusterConfigs,
};

export default exportFunctions;
