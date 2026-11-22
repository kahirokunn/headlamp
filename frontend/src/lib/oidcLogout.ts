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
import { getAppUrl } from '../helpers/getAppUrl';
import { getHeadlampAPIHeaders } from '../helpers/getHeadlampAPIHeaders';
import { IMPORT_REVISION_KEY_HEADER } from '../stateless/importRevisionKey';
import { getDiscoveredConnectionRevision } from './k8s/api/v2/discoveredConnectionRevisions';
import {
  assertCurrentDiscoveryConnection,
  createDiscoveryConnectionChangedError,
  DISCOVERY_CONNECTION_REVISION_HEADER,
  type DiscoveryConnectionRequest,
  getRoutingRevision,
  UNVERSIONED_DISCOVERY_CONNECTION,
} from './k8s/api/v2/discoveryConnection';
import { backendFetch } from './k8s/api/v2/fetch';
import { getOIDCImportHeaders } from './oidcLogin';

export function encodeClusterPath(cluster: string): string {
  return Base64.encodeURI(cluster);
}

/**
 * Requests an OIDC logout transition URL through the authenticated backend and navigates to it.
 *
 * @param cluster - The OIDC cluster whose provider session should be ended.
 * @param connection - The connection to keep through the logout transition.
 * @throws {Error} When the backend request fails or returns an invalid redirect URL.
 */
export async function redirectToOIDCLogout(
  cluster: string,
  connection: DiscoveryConnectionRequest = { revision: getDiscoveredConnectionRevision(cluster) }
): Promise<void> {
  assertCurrentDiscoveryConnection(cluster, connection.revision);
  const routingRevision =
    getRoutingRevision(connection.revision) || UNVERSIONED_DISCOVERY_CONNECTION;
  const imported = routingRevision.startsWith('import-')
    ? await getOIDCImportHeaders(cluster, connection.revision)
    : undefined;
  assertCurrentDiscoveryConnection(cluster, connection.revision);
  if (window.desktopApi?.oidcLogout) {
    if (imported) {
      await window.desktopApi.oidcLogout(
        cluster,
        routingRevision,
        imported.KUBECONFIG,
        imported['X-HEADLAMP-USER-ID'],
        imported[IMPORT_REVISION_KEY_HEADER]
      );
    } else {
      await window.desktopApi.oidcLogout(cluster, routingRevision);
    }
    return;
  }
  const response = await backendFetch(
    `/encoded-clusters/${encodeClusterPath(cluster)}/oidc-logout`,
    {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        [DISCOVERY_CONNECTION_REVISION_HEADER]: routingRevision,
        'Content-Type': 'application/json',
        ...getHeadlampAPIHeaders(),
        ...imported,
      },
    }
  );
  if (response.status === 409) throw createDiscoveryConnectionChangedError();
  const result: unknown = await response.json();
  assertCurrentDiscoveryConnection(cluster, connection.revision);

  if (
    !result ||
    typeof result !== 'object' ||
    !('redirectURL' in result) ||
    typeof result.redirectURL !== 'string' ||
    result.redirectURL === ''
  ) {
    throw new Error('OIDC logout response did not include a redirect URL');
  }

  const targetUrl = new URL(result.redirectURL, getAppUrl());
  if (targetUrl.protocol !== 'http:' && targetUrl.protocol !== 'https:') {
    throw new Error(`Invalid redirect URL protocol: ${targetUrl.protocol}`);
  }

  window.location.href = targetUrl.href;
}
