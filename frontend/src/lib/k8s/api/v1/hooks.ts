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

import React, { useContext } from 'react';
import { useHistory, useLocation } from 'react-router-dom';
import { getCluster, getSelectedClusters } from '../../../cluster';
import { SelectedClustersContext } from '../../SelectedClustersContext';
import {
  getDiscoveredConnectionRevision,
  useDiscoveredConnectionRevisions,
} from '../v2/discoveredConnectionRevisions';

export type CancellablePromise = Promise<() => void>;

/**
 * Get the currently selected cluster name.
 *
 * If more than one cluster is selected it will return:
 *  - On details pages: the cluster of the currently viewed resource
 *  - On any other page: one of the selected clusters
 *
 * To get all currently selected clusters please use {@link useSelectedClusters}
 *
 * @returns currently selected cluster
 */
export function useCluster() {
  const history = useHistory();

  const [cluster, setCluster] = React.useState(getCluster());

  React.useEffect(() => {
    // Listen to route changes
    return history.listen(() => {
      const newCluster = getCluster(history.location.pathname);
      // Update the state only when the cluster changes
      setCluster((currentCluster: string | null) =>
        newCluster !== currentCluster ? newCluster : currentCluster
      );
    });
  }, [history]);

  return cluster;
}

/**
 * Get a list of selected clusters. Updates when the cluster changes.
 *
 * @returns list of selected clusters. if no clusters are selected, an empty list is returned.
 */
export function useSelectedClusters(): string[] {
  const clusterInURL = useCluster();
  const location = useLocation();
  const maybeSelectedClusters = useContext(SelectedClustersContext);

  const clusterGroup = React.useMemo(() => {
    return getSelectedClusters([], location.pathname);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [clusterInURL, location.pathname]);

  return maybeSelectedClusters && maybeSelectedClusters.length > 0
    ? maybeSelectedClusters
    : clusterGroup;
}

/**
 * Hook to manage multiple cancellable API calls tied to the active cluster.
 *
 * @param apiCalls - functions returning cancellable promises for API calls.
 */
export function useConnectApi(...apiCalls: (() => CancellablePromise)[]) {
  useConnectionRequests(undefined, () => apiCalls.map(call => call()));
}

/** Keep callback-based reads bound to their effective cluster connection. */
export function useConnectionRequests(
  explicitCluster: string | undefined,
  start: (
    cluster: string | undefined,
    isCurrent: () => boolean,
    changed: boolean
  ) => CancellablePromise[],
  requestKey?: string
) {
  const routeCluster = useCluster();
  useClusterListRequests(
    [explicitCluster || routeCluster || undefined],
    ([cluster], isCurrent, changed) => start(cluster, isCurrent, changed),
    requestKey
  );
}

/** Start and cancel one group of reads when its cluster selection or any connection changes. */
export function useClusterListRequests(
  clusters: Array<string | undefined>,
  start: (
    clusters: Array<string | undefined>,
    isCurrent: () => boolean,
    changed: boolean
  ) => CancellablePromise[],
  requestKey?: string
) {
  const revisions = useDiscoveredConnectionRevisions();
  const connections = clusters.map(cluster => ({
    cluster,
    revision: cluster ? revisions.get(cluster) : undefined,
  }));
  const key = JSON.stringify([connections, requestKey]);
  const previous = React.useRef(key);

  React.useLayoutEffect(() => {
    const changed = previous.current !== key;
    previous.current = key;
    let active = true;
    const isCurrent = () =>
      active &&
      connections.every(
        ({ cluster, revision }) => !cluster || getDiscoveredConnectionRevision(cluster) === revision
      );
    const cancellables = start(clusters, isCurrent, changed);

    return () => {
      active = false;
      for (const promise of cancellables) promise.then(cancel => cancel());
    };
    // Callers recreate the callback and arrays on render. The key describes the selected requests.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
}
