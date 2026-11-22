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

import { getHeadlampAPIHeaders } from '../helpers/getHeadlampAPIHeaders';
import { findKubeconfigByClusterName } from '../stateless/findKubeconfigByClusterName';
import { getUserIdFromLocalStorage } from '../stateless/getUserIdFromLocalStorage';
import { getImportRevisionKey, IMPORT_REVISION_KEY_HEADER } from '../stateless/importRevisionKey';
import {
  assertCurrentDiscoveryConnection,
  createDiscoveryConnectionChangedError,
} from './k8s/api/v2/discoveryConnection';
import { backendFetch } from './k8s/api/v2/fetch';

export async function getOIDCImportHeaders(cluster: string, revision?: string) {
  if (!revision?.startsWith('import-')) return undefined;
  assertCurrentDiscoveryConnection(cluster, revision);
  const [kubeconfig, key] = await Promise.all([
    findKubeconfigByClusterName(cluster),
    getImportRevisionKey(),
  ]);
  assertCurrentDiscoveryConnection(cluster, revision);
  if (kubeconfig === null) throw createDiscoveryConnectionChangedError();
  return {
    KUBECONFIG: kubeconfig,
    [IMPORT_REVISION_KEY_HEADER]: key,
    'X-HEADLAMP-USER-ID': getUserIdFromLocalStorage(revision),
  };
}

/** Prepares an imported login without putting its kubeconfig or user ID in a URL. */
export async function prepareOIDCLogin(url: string, cluster: string, revision?: string) {
  assertCurrentDiscoveryConnection(cluster, revision);
  const headers = await getOIDCImportHeaders(cluster, revision);
  assertCurrentDiscoveryConnection(cluster, revision);
  if (!headers && !window.desktopApi?.oidcLogin) return url;
  const response = await backendFetch('/oidc' + new URL(url, window.location.href).search, {
    method: 'POST',
    redirect: 'error',
    headers: { ...getHeadlampAPIHeaders(), ...headers },
  });
  const result: { redirectURL?: string } | null = await response.json();
  assertCurrentDiscoveryConnection(cluster, revision);
  const destination = new URL(result?.redirectURL ?? '');
  if (
    !['https:', 'http:'].includes(destination.protocol) ||
    destination.username ||
    destination.password
  ) {
    throw new Error('Invalid OIDC login redirect');
  }
  return destination.href;
}
