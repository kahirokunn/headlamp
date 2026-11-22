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

import { Base64 } from 'js-base64';
import { getHeadlampAPIHeaders } from '../helpers/getHeadlampAPIHeaders';
import { request } from '../lib/k8s/api/v1/clusterRequests';
import type { Cluster } from '../lib/k8s/cluster';
import { getUserIdFromLocalStorage } from './getUserIdFromLocalStorage';
import { renewRetiredImport } from './records';
import { rollbackKubeconfigImport } from './rollbackKubeconfigImport';

export async function retireImportedConnection(
  cluster: Cluster | undefined,
  nextRevision?: string,
  renewStoredRecord = false
) {
  if (!cluster?.importRevision || cluster.importRevision === nextRevision) return;

  const retire = () =>
    request(
      `/stateless-connections/${Base64.encodeURI(cluster.name)}/retire`,
      {
        method: 'POST',
        isJSON: false,
        headers: {
          ...getHeadlampAPIHeaders(),
          'X-HEADLAMP-USER-ID': getUserIdFromLocalStorage(cluster.importRevision),
        },
      },
      false,
      false
    );
  try {
    await retire();
  } catch (error) {
    const status = (error as { status?: number } | null)?.status;
    if (status !== undefined && status !== 408 && status < 500) throw error;
    try {
      await retire();
    } catch (retryError) {
      // Either attempt could have committed before its response was lost.
      await rollbackKubeconfigImport(undefined, [cluster]);
      throw retryError;
    }
  }
  if (renewStoredRecord) {
    await renewRetiredImport(cluster).catch(error => {
      console.error('Could not renew the displaced kubeconfig record:', error);
    });
  }
}
