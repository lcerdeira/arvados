# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

class HealthcheckController < ApplicationController
  skip_around_action :thread_clear
  skip_around_action :set_thread_api_token
  skip_around_action :require_thread_api_token
  skip_before_action :ensure_arvados_api_exists
  skip_before_action :accept_uuid_as_id_param
  skip_before_action :check_user_agreements
  skip_before_action :check_user_profile
  skip_before_action :load_filters_and_paging_params
  skip_before_action :find_object_by_uuid

  before_action :check_auth_header

  def check_auth_header
    mgmt_token = Rails.configuration.ManagementToken
    auth_header = request.headers['Authorization']

    if mgmt_token.empty?
      render :json => {:errors => "disabled"}, :status => 404
    elsif !auth_header
      render :json => {:errors => "authorization required"}, :status => 401
    elsif auth_header != 'Bearer '+mgmt_token
      render :json => {:errors => "authorization error"}, :status => 403
    end
  end

  def ping
    resp = {"health" => "OK"}
    render json: resp
  end
end
