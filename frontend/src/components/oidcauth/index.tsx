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

import Typography from '@mui/material/Typography';
import { useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useDispatch } from 'react-redux';
import { useHistory, useLocation } from 'react-router-dom';
import { useDiscoveredConnectionRevisions } from '../../lib/k8s/api/v2/discoveredConnectionRevisions';
import {
  DISCOVERY_CONNECTION_CHANGED,
  DISCOVERY_CONNECTION_REVISION_PARAM,
} from '../../lib/k8s/api/v2/discoveryConnection';
import { createRouteURL } from '../../lib/router/createRouteURL';
import { getEffectiveCluster, setClusterUseToken } from '../../redux/configSlice';
import { useTypedSelector } from '../../redux/hooks';
import { oidcAuthStatusKey, oidcSessionRevision } from './constants';

const handledAuthUrls = new Set<string>();

export function resetHandledAuthUrls() {
  handledAuthUrls.clear();
}

/** Signals OIDC authentication completion via localStorage for the popup handler or navigates in full-page mode. */
function OIDCAuth() {
  const { search } = useLocation();
  const history = useHistory();
  const dispatch = useDispatch();
  const params = new URLSearchParams(search);
  const cluster = params.get('cluster');
  const effectiveCluster = useTypedSelector(state =>
    getEffectiveCluster(state.config, cluster ?? '')
  );
  const revision = params.get(DISCOVERY_CONNECTION_REVISION_PARAM);
  const configLoaded = useTypedSelector(state => state.config.clusters !== null);
  const checkingConnection = !!cluster && !!revision && !configLoaded;
  const currentRevision = oidcSessionRevision(
    useDiscoveredConnectionRevisions().get(cluster ?? '')
  );
  const connectionChanged =
    !!cluster &&
    !!(revision || effectiveCluster) &&
    oidcSessionRevision(revision) !== currentRevision;
  const statusKey = oidcAuthStatusKey(cluster, revision);
  const { t } = useTranslation();

  // Set by the backend when the login was started from OauthPopup. Not derived from
  // window.opener, which an IdP's Cross-Origin-Opener-Policy can sever.
  const isPopup = params.get('popup') === 'true';
  const storageError = useMemo(() => {
    if (!cluster || isPopup || checkingConnection || connectionChanged) {
      return null;
    }
    try {
      sessionStorage.setItem(`oidc-login-attempted.${cluster}`, revision || 'true');
      return null;
    } catch {
      return t(
        'Unable to access browser storage to complete authentication. Please enable cookies and storage in your browser settings and try again.'
      );
    }
  }, [cluster, checkingConnection, connectionChanged, isPopup, revision, t]);

  useEffect(() => {
    if (
      storageError ||
      checkingConnection ||
      connectionChanged ||
      !cluster ||
      handledAuthUrls.has(search)
    ) {
      return;
    }

    handledAuthUrls.add(search);

    if (isPopup) {
      localStorage.setItem(statusKey, 'success');
    }

    if (effectiveCluster && effectiveCluster.useToken !== false) {
      dispatch(setClusterUseToken({ clusterName: cluster, useToken: false }));
    }

    if (!isPopup) {
      let returnUrl = '';
      try {
        returnUrl = sessionStorage.getItem('oidc_return_url') || '';
        if (returnUrl) {
          sessionStorage.removeItem('oidc_return_url');
        }
      } catch (e) {
        console.error('Failed to get return URL from sessionStorage', e);
      }

      if (!returnUrl && cluster) {
        returnUrl = createRouteURL('cluster', { cluster }) || `/c/${cluster}/`;
      }

      if (!returnUrl) {
        returnUrl = '/';
      }

      history.replace(returnUrl);
    }
  }, [
    cluster,
    effectiveCluster,
    checkingConnection,
    connectionChanged,
    dispatch,
    history,
    isPopup,
    search,
    statusKey,
    storageError,
  ]);

  if (storageError || (!checkingConnection && connectionChanged)) {
    return (
      <Typography color="error" role="alert">
        {storageError || DISCOVERY_CONNECTION_CHANGED}
      </Typography>
    );
  }

  return <Typography color="textPrimary">{t('Redirecting to main page…')}</Typography>;
}

export default OIDCAuth;
