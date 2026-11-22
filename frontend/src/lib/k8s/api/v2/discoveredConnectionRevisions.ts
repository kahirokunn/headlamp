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

import { createSelector } from '@reduxjs/toolkit';
import { isEqual } from 'lodash';
import { useSyncExternalStore } from 'react';
import { resolvedAllowedNamespacesKey } from '../../../../helpers/resolvedAllowedNamespacesKey';
import type { RootState } from '../../../../redux/stores/store';
import store from '../../../../redux/stores/store';

// Guard requests between removal and replacement without retaining every
// registration the browser has seen. Newly removed names take precedence.
const removedDiscoveryRevision = 'removed';
const MAX_REMOVED_CONNECTIONS = 128;

const selectConfiguredRevisions = createSelector(
  [
    (state: RootState) => state.config.clusters,
    (state: RootState) => state.config.statelessClusters,
  ],
  (clusters, statelessClusters) =>
    new Map(
      Object.entries({ ...clusters, ...statelessClusters }).map(([name, cluster]) => [
        name,
        statelessClusters?.[name]
          ? cluster.importRevision ?? null
          : cluster.meta_data?.discoveryOrigin
          ? cluster.meta_data.discoveryConnectionRevision ?? ''
          : null,
      ])
    ),
  { memoizeOptions: { resultEqualityCheck: isEqual } }
);

let configuredRevisions = new Map<string, string | null>();
let localRevisions = new Map<string, string>();
let nextLocalRevision = 0;
let removedNamesEvicted = false;
let stopTrackingRevisions: (() => void) | undefined;

function evictResolvedNamespaces(cluster: string) {
  try {
    localStorage.removeItem(resolvedAllowedNamespacesKey(cluster));
  } catch {
    // Connection tracking also runs when browser policy disables Web Storage.
  }
}

function updateDiscoveryRevisions() {
  const current = selectConfiguredRevisions(store.getState());
  if (current !== configuredRevisions) {
    // Revisiting an earlier endpoint must fetch fresh data and start a new watch.
    // A persistent namespace result for an ordinary connection cannot be reused
    // after the same name has been served by a discovered or imported connection.
    for (const [cluster, revision] of configuredRevisions) {
      if (revision !== null && revision !== current.get(cluster)) {
        evictResolvedNamespaces(cluster);
      }
    }
    const revisions = new Map<string, string>();
    for (const [cluster, revision] of current) {
      if (revision === null) {
        // Once a removal has been forgotten, a newly appearing ordinary name may
        // have served an earlier connection. Give it a fresh local epoch.
        if (
          localRevisions.has(cluster) ||
          (removedNamesEvicted && !configuredRevisions.has(cluster))
        ) {
          const previous = configuredRevisions.get(cluster) === null && localRevisions.get(cluster);
          revisions.set(cluster, previous || `unversioned:${++nextLocalRevision}`);
        }
        continue;
      }
      if (configuredRevisions.get(cluster) !== revision) {
        evictResolvedNamespaces(cluster);
      }
      const previous = revision === configuredRevisions.get(cluster) && localRevisions.get(cluster);
      revisions.set(cluster, previous || (revision ? `${revision}:${++nextLocalRevision}` : ''));
    }
    let retainedRemovals = 0;
    for (const cluster of localRevisions.keys()) {
      if (current.has(cluster)) continue;
      if (retainedRemovals === MAX_REMOVED_CONNECTIONS) {
        removedNamesEvicted = true;
        break;
      }
      revisions.set(cluster, removedDiscoveryRevision);
      retainedRemovals++;
    }
    configuredRevisions = current;
    localRevisions = revisions;
  }
}

function getDiscoveryRevisions() {
  // Query caches can outlive their views. Track every transition after the first
  // use, even when nothing is mounted to observe updates to the store.
  stopTrackingRevisions ??= store.subscribe(updateDiscoveryRevisions);
  updateDiscoveryRevisions();
  return localRevisions;
}

import.meta.hot?.dispose(() => stopTrackingRevisions?.());

export function getDiscoveredConnectionRevision(cluster: string) {
  return getDiscoveryRevisions().get(cluster);
}

export function getImportedConnectionRevision(cluster: string) {
  const revision = getDiscoveredConnectionRevision(cluster);
  return revision?.startsWith('import-') ? revision : undefined;
}

export function useDiscoveredConnectionRevisions() {
  return useSyncExternalStore(store.subscribe, getDiscoveryRevisions);
}
