class JobWorkUnit < ProxyWorkUnit
  def children
    return self.my_children if self.my_children

    # Jobs components
    items = []
    components = get(:components)
    uuids = components.andand.collect {|_, v| v}
    return items if (!uuids or uuids.empty?)

    rcs = {}
    uuids.each do |u|
      r = ArvadosBase::resource_class_for_uuid(u)
      rcs[r] = [] unless rcs[r]
      rcs[r] << u
    end
    rcs.each do |rc, ids|
      rc.where(uuid: ids).each do |obj|
        items << obj.work_unit(components.key(obj.uuid))
      end
    end

    self.my_children = items
  end

  def parameters
    get(:script_parameters)
  end

  def repository
    get(:repository)
  end

  def script
    get(:script)
  end

  def script_version
    get(:script_version)
  end

  def supplied_script_version
    get(:supplied_script_version)
  end

  def docker_image
    get(:docker_image_locator)
  end

  def nondeterministic
    get(:nondeterministic)
  end

  def child_summary
    if children.any?
      super
    else
      get(:tasks_summary)
    end
  end

  def can_cancel?
    true
  end

  def uri
    uuid = get(:uuid)
    "/jobs/#{uuid}"
  end

  def title
    "job"
  end
end
