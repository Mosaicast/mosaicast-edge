// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Mosaicast Authors

// Theme scripts react to the runtime's events; they never decide phases or read the device clock.
'use strict';
document.addEventListener('edge:party', () => {
  document.querySelector('.mx-party h1')?.classList.add('is-party');
});
