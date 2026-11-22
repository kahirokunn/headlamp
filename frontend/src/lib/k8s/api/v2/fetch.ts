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

import { addBackstageAuthHeaders } from '../../../../helpers/addBackstageAuthHeaders';
import { getAppUrl } from '../../../../helpers/getAppUrl';
import { getHeadlampAPIHeaders } from '../../../../helpers/getHeadlampAPIHeaders';
import { findKubeconfigByClusterName } from '../../../../stateless/findKubeconfigByClusterName';
import { getUserIdFromLocalStorage } from '../../../../stateless/getUserIdFromLocalStorage';
import {
  getImportRevisionKey,
  IMPORT_REVISION_KEY_HEADER,
} from '../../../../stateless/importRevisionKey';
import { ApiError } from './ApiError';
import { getDiscoveredConnectionRevision } from './discoveredConnectionRevisions';
import type { DiscoveryConnectionRequest } from './discoveryConnection';
import {
  assertCurrentDiscoveryConnection,
  DISCOVERY_CONNECTION_REVISION_HEADER,
  UNVERSIONED_DISCOVERY_CONNECTION,
  withDiscoveryConnectionPath,
} from './discoveryConnection';
import { makeUrl } from './makeUrl';

// @deprecated BASE_HTTP_URL is deprecated for Electron apps with custom ports.
// It's evaluated at module load time, before window.headlampBackendPort is set.
// Use getAppUrl() directly instead for runtime port configuration.
export const BASE_HTTP_URL = getAppUrl();

/**
 * Simple wrapper around Fetch function
 * Sends a request to the backend
 *
 * @param url - URL path
 * @param init - options parameter for the Fetch function
 *
 * @returns fetch Response
 */
export async function backendFetch(url: string | URL, init: RequestInit = {}) {
  // Always include credentials
  init.credentials = 'include';
  init.headers = addBackstageAuthHeaders(init.headers);
  const headers = new Headers(init.headers);
  if (headers.has('KUBECONFIG') && !headers.has(IMPORT_REVISION_KEY_HEADER)) {
    headers.set(IMPORT_REVISION_KEY_HEADER, await getImportRevisionKey());
    init.headers = headers;
  }
  const response = await fetch(makeUrl([getAppUrl(), url]), init);

  // The backend signals through this header that it wants a reload.
  // See plugins.go
  const headerVal = response.headers.get('X-Reload');
  if (headerVal && headerVal.indexOf('reload') !== -1) {
    window.location.reload();
  }

  if (!response.ok) {
    // Try to parse error message from response
    let maybeErrorMessage: string | undefined;
    try {
      if (typeof response.text === 'function') {
        const text = await response.text();
        try {
          const body = JSON.parse(text);
          if (typeof body === 'string') {
            maybeErrorMessage = body;
          } else if (body && typeof body === 'object') {
            const candidate = body.message || body.error_description || body.error;
            if (typeof candidate === 'string') {
              maybeErrorMessage = candidate;
            }
          }
        } catch {
          maybeErrorMessage = text.trim() || undefined;
        }
      } else if (typeof response.json === 'function') {
        const body = await response.json();
        if (typeof body === 'string') {
          maybeErrorMessage = body;
        } else if (body && typeof body === 'object') {
          const candidate = body.message || body.error_description || body.error;
          if (typeof candidate === 'string') {
            maybeErrorMessage = candidate;
          }
        }
      }
    } catch (e) {
      console.debug(
        `Failed to parse error response body for ${url} (status ${response.status}):`,
        e
      );
    }

    throw new ApiError(maybeErrorMessage ?? 'Unreachable', { status: response.status });
  }

  return response;
}

/**
 * A wrapper around Fetch function
 * Allows sending requests to a particular cluster
 *
 * @param url - URL path
 * @param init - same as second parameter of the Fetch function
 * @param init.cluster - name of the cluster
 *
 * @returns fetch Response
 */
export async function clusterFetch(
  url: string | URL,
  {
    cluster,
    expectedDiscoveryConnection: boundConnection,
    ...init
  }: RequestInit & {
    cluster: string;
    expectedDiscoveryConnection?: DiscoveryConnectionRequest;
  }
) {
  init.headers = new Headers(init.headers);
  if (cluster) {
    for (const [name, value] of Object.entries(getHeadlampAPIHeaders())) {
      init.headers.set(name, value);
    }
  }

  const expectedDiscoveryConnection =
    boundConnection ??
    (cluster ? { revision: getDiscoveredConnectionRevision(cluster) } : undefined);

  // Set stateless kubeconfig if exists
  const kubeconfig = cluster ? await findKubeconfigByClusterName(cluster) : null;
  if (kubeconfig !== null) {
    const userID = getUserIdFromLocalStorage(expectedDiscoveryConnection?.revision);
    init.headers.set('KUBECONFIG', kubeconfig);
    init.headers.set('X-HEADLAMP-USER-ID', userID);
  }

  if (init.headers.has('KUBECONFIG')) {
    init.headers.set(IMPORT_REVISION_KEY_HEADER, await getImportRevisionKey());
  }

  if (expectedDiscoveryConnection) {
    assertCurrentDiscoveryConnection(cluster, expectedDiscoveryConnection.revision);
    if (cluster && expectedDiscoveryConnection.revision === undefined) {
      init.headers.set(DISCOVERY_CONNECTION_REVISION_HEADER, UNVERSIONED_DISCOVERY_CONNECTION);
    }
  }

  const urlParts = cluster ? ['clusters', cluster, url] : [url];

  try {
    const path = withDiscoveryConnectionPath(
      makeUrl(urlParts),
      expectedDiscoveryConnection?.revision === undefined ? undefined : expectedDiscoveryConnection
    );
    const response = await backendFetch(path, init);

    return response;
  } catch (e) {
    if (e instanceof ApiError) {
      e.cluster = cluster;
    }
    throw e;
  } finally {
    const method = (init.method ?? 'GET').toUpperCase();
    if (expectedDiscoveryConnection && (method === 'GET' || method === 'HEAD')) {
      assertCurrentDiscoveryConnection(cluster, expectedDiscoveryConnection.revision);
    }
  }
}
