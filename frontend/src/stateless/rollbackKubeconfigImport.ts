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
import { setStatelessConfig } from '../redux/configSlice';
import store from '../redux/stores/store';
import {
  discardPendingKubeconfigRecord,
  renewRetiredImport,
  sameImportedSelection,
} from './records';

export async function rollbackKubeconfigImport(
  recordId: number | undefined,
  previous: Array<Cluster | undefined>
) {
  const cleanup = await Promise.allSettled([
    recordId !== undefined ? discardPendingKubeconfigRecord(recordId) : undefined,
    ...previous.map(async cluster => {
      if (!cluster) return;
      const importRevision = await renewRetiredImport(cluster);
      if (!importRevision) return;

      const current = store.getState().config.statelessClusters;
      const selected = current?.[cluster.name];
      if (!selected || !sameImportedSelection(selected, cluster)) return;
      store.dispatch(
        setStatelessConfig({
          statelessClusters: {
            ...current,
            [cluster.name]: { ...selected, importRevision, useToken: undefined },
          },
        })
      );
    }),
  ]);
  cleanup.forEach(result => {
    if (result.status === 'rejected') {
      console.error('Error rolling back the stored kubeconfig:', result.reason);
    }
  });
}
