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

import { type ConfigState, setStatelessConfig } from '../redux/configSlice';
import store from '../redux/stores/store';
import { type DisplacedContext, publishKubeconfigRecord, sameImportedSelection } from './records';

/** Commit the selected connections and their stored kubeconfig together. */
export async function installKubeconfigRecord(
  recordId: number | undefined,
  displaced: Array<DisplacedContext | undefined>,
  names: string[],
  select: () => Pick<ConfigState, 'statelessClusters'>
) {
  if (recordId === undefined) {
    store.dispatch(setStatelessConfig(select()));
    return;
  }

  let original: ConfigState['statelessClusters'] = null;
  let selected: ConfigState['statelessClusters'] = null;
  await publishKubeconfigRecord(
    recordId,
    displaced,
    () => {
      original = store.getState().config.statelessClusters;
      selected = select().statelessClusters;
    },
    names
  );

  // Redux observers can now read the published record. Preserve independent edits
  // made while the transaction finished.
  const current = store.getState().config.statelessClusters;
  const next = { ...current };
  let superseded = false;
  for (const name of names) {
    if (!sameImportedSelection(current?.[name], original?.[name])) {
      if (!sameImportedSelection(current?.[name], selected?.[name])) superseded = true;
      continue;
    }
    const cluster = selected?.[name];
    if (cluster) next[name] = cluster;
    else delete next[name];
  }
  store.dispatch(setStatelessConfig({ statelessClusters: next }));
  if (superseded) throw new Error('The selected kubeconfig changed during import');
}
