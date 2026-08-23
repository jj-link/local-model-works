(function () {
  "use strict";

  // Individual physical devices. No cluster entries.
  var devices = [
    { id: "rtx-workstation", name: "RTX workstation", short: "RTX workstation", detail: "Local NVIDIA GPU · 1 node", rdma: false, installedRecipes: ["lmw-demo-serve", "lmw-demo-egress"] },
    { id: "spark1", name: "DGX Spark · spark1", short: "spark1", detail: "GB10 · 1 node", rdma: false, installedRecipes: ["lmw-demo-profiles"] },
    { id: "spark2", name: "DGX Spark · spark2", short: "spark2", detail: "GB10 · 1 node · RDMA fabric", rdma: true, installedRecipes: ["demo-rdma-pair"] },
    { id: "spark3", name: "DGX Spark · spark3", short: "spark3", detail: "GB10 · 1 node · RDMA fabric", rdma: true, installedRecipes: ["demo-rdma-pair"] },
  ];

  function deviceState(recipe, device) {
    if (recipe.compatibility.rdma && !device.rdma) {
      return { state: "incompatible", label: "Incompatible", detail: "No RDMA fabric on this device" };
    }
    if (device.installedRecipes.indexOf(recipe.id) !== -1) {
      return { state: "installed", label: "Installed", detail: "Installed on this device" };
    }
    return { state: "selectable", label: "Select", detail: "Available for installation" };
  }

  function introText(recipe) {
    var needed = recipe.compatibility.node_count;
    if (needed > 1) {
      return "This recipe runs on " + needed + " nodes. Select " + needed + " compatible devices below.";
    }
    return "Select one destination for this recipe.";
  }

  function markup(recipe) {
    var needed = recipe.compatibility.node_count;
    var cardHtml = devices.map(function (device) {
      var status = deviceState(recipe, device);
      var selectable = status.state === "selectable";
      return '<button type="button" class="lmw-install-target is-' + status.state + '" data-install-target="' + device.id + '" aria-pressed="false"' + (selectable ? "" : " disabled") + '>' +
        '<span class="lmw-install-target-top"><span class="lmw-device-dot" aria-hidden="true"></span><b>' + device.name + '</b><span class="lmw-install-target-choice" aria-hidden="true">' + status.label + "</span></span>" +
        '<span class="lmw-install-target-detail">' + device.detail + "</span>" +
        '<span class="lmw-install-target-fit">' + status.detail + "</span>" +
      "</button>";
    }).join("");

    return '<section class="lmw-install" data-install-chooser>' +
      '<header class="lmw-install-head"><span>INSTALL RECIPE</span><h3>Choose hardware</h3><p>' + introText(recipe) + "</p></header>" +
      '<div class="lmw-install-targets">' + cardHtml + "</div>" +
      '<div class="lmw-install-action" data-install-action>' +
        '<p data-install-summary>' + (needed > 1 ? "Select " + needed + " devices to continue." : "Select one device to continue.") + "</p>" +
        '<button type="button" data-install-start disabled>Confirm hardware</button>' +
      "</div>" +
      '<div class="lmw-install-started" data-install-started hidden></div>' +
    "</section>";
  }

  function wire(root, recipe) {
    var chooser = root.querySelector("[data-install-chooser]");
    if (!chooser) return;

    var needed = recipe.compatibility.node_count;
    var chosen = [];
    var summary = chooser.querySelector("[data-install-summary]");
    var start = chooser.querySelector("[data-install-start]");
    var cards = Array.prototype.slice.call(chooser.querySelectorAll("[data-install-target]"));

    function refresh() {
      cards.forEach(function (card) {
        var on = chosen.indexOf(card.dataset.installTarget) !== -1;
        card.classList.toggle("is-selected", on);
        card.setAttribute("aria-pressed", String(on));
        var chip = card.querySelector(".lmw-install-target-choice");
        if (chip && card.classList.contains("is-selectable")) chip.textContent = on ? "Selected ✓" : "Select";
      });
      if (chosen.length === 0) {
        summary.textContent = needed > 1 ? "Select " + needed + " devices to continue." : "Select one device to continue.";
        start.textContent = "Confirm hardware";
        start.disabled = true;
      } else {
        var names = chosen.map(function (id) {
          var d = devices.find(function (x) { return x.id === id; });
          return d ? d.short : id;
        });
        summary.textContent = recipe.name + " → " + names.join(" + ");
        start.textContent = "Confirm on " + names.join(" + ");
        start.disabled = chosen.length !== needed;
      }
    }

    cards.forEach(function (card) {
      if (card.disabled) return;
      card.addEventListener("click", function () {
        var id = card.dataset.installTarget;
        var at = chosen.indexOf(id);
        if (at !== -1) {
          chosen.splice(at, 1);
        } else if (chosen.length < needed) {
          chosen.push(id);
        }
        refresh();
      });
    });

    start.addEventListener("click", function () {
      if (chosen.length !== needed) return;
      cards.forEach(function (card) { card.disabled = true; });
      chooser.querySelector("[data-install-action]").hidden = true;
      var started = chooser.querySelector("[data-install-started]");
      started.hidden = false;
      var names = chosen.map(function (id) {
        var d = devices.find(function (x) { return x.id === id; });
        return d ? d.short : id;
      });
      started.innerHTML =
        '<span class="lmw-install-started-kicker">DEMO CONFIRMED</span>' +
        '<h4>' + recipe.name + " on " + names.join(" + ") + "</h4>" +
        '<p>This is a visual prototype. No installation or device command was run.</p>';
    });
  }
  function installedDevices(recipe) {
    return devices.filter(function (device) {
      return device.installedRecipes.indexOf(recipe.id) !== -1;
    });
  }

  function installedDeviceCount(recipe) {
    return installedDevices(recipe).length;
  }


  window.LMWInstall = {
    markup: markup,
    deviceState: deviceState,
    installedDevices: installedDevices,
    installedDeviceCount: installedDeviceCount,
    wire: wire,
  };
})();
